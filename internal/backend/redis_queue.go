package backend

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	pendingKey  = "video-server:outbox"
	inFlightKey = "video-server:outbox:in-flight"
)

// RedisQueue keeps the outbox in two Redis lists: what is waiting and what is being delivered.
type RedisQueue struct {
	client *redis.Client
}

// NewRedisQueue builds a queue on a Redis connection.
func NewRedisQueue(client *redis.Client) *RedisQueue {
	return &RedisQueue{client: client}
}

func (q *RedisQueue) Push(ctx context.Context, item []byte) error {
	return q.client.RPush(ctx, pendingKey, item).Err()
}

func (q *RedisQueue) Claim(ctx context.Context, wait time.Duration) ([]byte, error) {
	item, err := q.client.BLMove(ctx, pendingKey, inFlightKey, "LEFT", "RIGHT", wait).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return item, err
}

func (q *RedisQueue) Ack(ctx context.Context, item []byte) error {
	return q.client.LRem(ctx, inFlightKey, 1, item).Err()
}

func (q *RedisQueue) Release(ctx context.Context, item []byte) error {
	_, err := q.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.LPush(ctx, pendingKey, item)
		pipe.LRem(ctx, inFlightKey, 1, item)
		return nil
	})
	return err
}

func (q *RedisQueue) Recover(ctx context.Context) error {
	for {
		// Newest first onto the front, so the recovered items keep their original order
		err := q.client.LMove(ctx, inFlightKey, pendingKey, "RIGHT", "LEFT").Err()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
