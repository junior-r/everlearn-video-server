package feed

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// A class's feed outlives its longest room token, then goes away on its own.
const retention = 6 * time.Hour

// RedisLog keeps each stream of a room in a Redis list, trimmed to its newest packets.
type RedisLog struct {
	client *redis.Client
}

// NewRedisLog builds a log on a Redis connection.
func NewRedisLog(client *redis.Client) *RedisLog {
	return &RedisLog{client: client}
}

func key(roomName string, stream Stream) string {
	return "video-server:feed:" + roomName + ":" + stream.Name
}

func (l *RedisLog) Append(ctx context.Context, roomName string, stream Stream, packet Packet) error {
	item, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	list := key(roomName, stream)
	_, err = l.client.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.RPush(ctx, list, item)
		pipe.LTrim(ctx, list, int64(-stream.Keep), -1)
		pipe.Expire(ctx, list, retention)
		return nil
	})
	return err
}

func (l *RedisLog) Recent(ctx context.Context, roomName string) ([]Packet, error) {
	var packets []Packet
	for _, stream := range Streams {
		items, err := l.client.LRange(ctx, key(roomName, stream), 0, -1).Result()
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			var packet Packet
			if err := json.Unmarshal([]byte(item), &packet); err != nil {
				return nil, err
			}
			packets = append(packets, packet)
		}
	}
	return packets, nil
}
