package redis

//go:generate go run ../internal/genredis -out client_gen.go -hand client.go

import (
	"context"
	"errors"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// ErrNotFound is returned when a key, field, member or list element does not
// exist. It replaces go-redis's redis.Nil so callers can use errors.Is without
// importing go-redis.
var ErrNotFound = errors.New("redis: not found")

// Types from go-redis that appear in Client method signatures, re-exported so
// ordinary code does not need to import go-redis directly.
type (
	// Z is a sorted-set member with its score.
	Z = goredis.Z
	// Pipeliner queues commands and sends them together. See Client.Pipelined.
	Pipeliner = goredis.Pipeliner
	// Tx is a WATCH transaction. See Client.Watch.
	Tx = goredis.Tx
	// PubSub is a subscription. The caller must Close it.
	PubSub = goredis.PubSub
)

// Client wraps a go-redis client with plain methods that return values and
// errors instead of go-redis command objects. It has a method for every command
// go-redis implements (620 as of go-redis v9.23.0): strings, keys, counters, hashes,
// lists, sets, sorted sets, streams, bitmaps, geo, HyperLogLog, scripts and
// functions, pub/sub, cluster and server administration, and the JSON, Search,
// TimeSeries, Bloom-family and vector-set module commands.
//
// The common commands are written by hand in this file; the rest are generated
// into client_gen.go (run `go generate ./redis` after changing the go-redis
// version, and a test fails if one is missing). A nil reply becomes ErrNotFound.
// Methods whose documentation says "Dangerous" can delete data or disturb other
// clients, for example FlushAll, Shutdown, ConfigSet, ACL changes and Keys; check
// before calling them against a shared server. Not wrapped: Monitor (a streaming
// command; use Native), and Quit, Sync and JSONDebugMemory, which go-redis itself
// implements as panics.
//
// Types that appear in arguments and results, such as XAddArgs or GeoLocation,
// come from github.com/redis/go-redis/v9. Closing a Client closes Native.
type Client struct {
	// Native is the underlying go-redis client for any command not wrapped here.
	Native *goredis.Client
}

// Wrap returns a Client around rdb, typically the result of New:
//
//	rdb, err := redis.New(ctx, redis.Config{URL: url})
//	if err != nil { return err }
//	c := redis.Wrap(rdb)
//	defer c.Close()
func Wrap(rdb *goredis.Client) *Client { return &Client{Native: rdb} }

// Ping checks that the server answers.
func (c *Client) Ping(ctx context.Context) error { return c.Native.Ping(ctx).Err() }

// Close closes the underlying client and its connections.
func (c *Client) Close() error { return c.Native.Close() }

// notFound converts go-redis's Nil reply into ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, goredis.Nil) {
		return ErrNotFound
	}
	return err
}

func get[T any](v T, err error) (T, error) { return v, notFound(err) }

// ---- strings ----

// Set stores value at key. A ttl of 0 means no expiry.
func (c *Client) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	return c.Native.Set(ctx, key, value, ttl).Err()
}

// SetNX stores value only if key does not exist and reports whether it did.
// Useful as a simple lock or "first writer wins"; ttl of 0 means no expiry.
func (c *Client) SetNX(ctx context.Context, key string, value any, ttl time.Duration) (bool, error) {
	return c.Native.SetNX(ctx, key, value, ttl).Result()
}

// Get returns the string at key, or ErrNotFound.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	return get(c.Native.Get(ctx, key).Result())
}

// GetDel returns the string at key and deletes it, or ErrNotFound.
func (c *Client) GetDel(ctx context.Context, key string) (string, error) {
	return get(c.Native.GetDel(ctx, key).Result())
}

// MGet returns the values of the keys that exist. Missing keys are absent from the map.
func (c *Client) MGet(ctx context.Context, keys ...string) (map[string]string, error) {
	vals, err := c.Native.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vals))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			out[keys[i]] = s
		}
	}
	return out, nil
}

// MSet stores several key/value pairs at once.
func (c *Client) MSet(ctx context.Context, pairs map[string]any) error {
	return c.Native.MSet(ctx, pairs).Err()
}

// Append appends value to the string at key and returns the new length.
func (c *Client) Append(ctx context.Context, key, value string) (int64, error) {
	return c.Native.Append(ctx, key, value).Result()
}

// ---- counters ----

// Incr adds 1 to the integer at key (created as 0 if missing) and returns the result.
func (c *Client) Incr(ctx context.Context, key string) (int64, error) {
	return c.Native.Incr(ctx, key).Result()
}

// IncrBy adds n to the integer at key and returns the result.
func (c *Client) IncrBy(ctx context.Context, key string, n int64) (int64, error) {
	return c.Native.IncrBy(ctx, key, n).Result()
}

// Decr subtracts 1 from the integer at key and returns the result.
func (c *Client) Decr(ctx context.Context, key string) (int64, error) {
	return c.Native.Decr(ctx, key).Result()
}

// DecrBy subtracts n from the integer at key and returns the result.
func (c *Client) DecrBy(ctx context.Context, key string, n int64) (int64, error) {
	return c.Native.DecrBy(ctx, key, n).Result()
}

// IncrByFloat adds f to the number at key and returns the result.
func (c *Client) IncrByFloat(ctx context.Context, key string, f float64) (float64, error) {
	return c.Native.IncrByFloat(ctx, key, f).Result()
}

// ---- keys ----

// Del deletes keys and returns how many existed.
func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	return c.Native.Del(ctx, keys...).Result()
}

// Exists returns how many of keys exist.
func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.Native.Exists(ctx, keys...).Result()
}

// Expire sets a time to live on key and reports whether the key existed.
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return c.Native.Expire(ctx, key, ttl).Result()
}

// Persist removes the expiry from key and reports whether there was one.
func (c *Client) Persist(ctx context.Context, key string) (bool, error) {
	return c.Native.Persist(ctx, key).Result()
}

// TTL returns the remaining time to live of key. It returns -1 if the key has
// no expiry and -2 if the key does not exist.
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.Native.TTL(ctx, key).Result()
}

// Rename renames key to newKey, replacing newKey if it exists.
func (c *Client) Rename(ctx context.Context, key, newKey string) error {
	return c.Native.Rename(ctx, key, newKey).Err()
}

// Type returns the Redis type of key: "string", "hash", "list", "set", "zset",
// "stream", or "none" if it does not exist.
func (c *Client) Type(ctx context.Context, key string) (string, error) {
	return c.Native.Type(ctx, key).Result()
}

// Scan returns all keys matching the glob pattern (for example "user:*"), using
// SCAN so the server is never blocked. count is a hint for the batch size.
// Keys written while scanning may or may not be returned.
func (c *Client) Scan(ctx context.Context, match string, count int64) ([]string, error) {
	var keys []string
	it := c.Native.Scan(ctx, 0, match, count).Iterator()
	for it.Next(ctx) {
		keys = append(keys, it.Val())
	}
	return keys, it.Err()
}

// ---- hashes ----

// HSet sets fields on the hash at key.
func (c *Client) HSet(ctx context.Context, key string, fields map[string]any) error {
	return c.Native.HSet(ctx, key, fields).Err()
}

// HGet returns one field of the hash at key, or ErrNotFound.
func (c *Client) HGet(ctx context.Context, key, field string) (string, error) {
	return get(c.Native.HGet(ctx, key, field).Result())
}

// HGetAll returns every field of the hash at key. A missing hash gives an empty map.
func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.Native.HGetAll(ctx, key).Result()
}

// HDel deletes fields and returns how many existed.
func (c *Client) HDel(ctx context.Context, key string, fields ...string) (int64, error) {
	return c.Native.HDel(ctx, key, fields...).Result()
}

// HExists reports whether field exists in the hash at key.
func (c *Client) HExists(ctx context.Context, key, field string) (bool, error) {
	return c.Native.HExists(ctx, key, field).Result()
}

// HIncrBy adds n to an integer field and returns the result.
func (c *Client) HIncrBy(ctx context.Context, key, field string, n int64) (int64, error) {
	return c.Native.HIncrBy(ctx, key, field, n).Result()
}

// HLen returns the number of fields in the hash at key.
func (c *Client) HLen(ctx context.Context, key string) (int64, error) {
	return c.Native.HLen(ctx, key).Result()
}

// ---- lists ----

// LPush inserts values at the head of the list and returns its length.
func (c *Client) LPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.Native.LPush(ctx, key, values...).Result()
}

// RPush appends values at the tail of the list and returns its length.
func (c *Client) RPush(ctx context.Context, key string, values ...any) (int64, error) {
	return c.Native.RPush(ctx, key, values...).Result()
}

// LPop removes and returns the head of the list, or ErrNotFound if it is empty.
func (c *Client) LPop(ctx context.Context, key string) (string, error) {
	return get(c.Native.LPop(ctx, key).Result())
}

// RPop removes and returns the tail of the list, or ErrNotFound if it is empty.
func (c *Client) RPop(ctx context.Context, key string) (string, error) {
	return get(c.Native.RPop(ctx, key).Result())
}

// BLPop waits up to timeout for an element at the head of any of keys and
// returns the key it came from and the element. It returns ErrNotFound when the
// timeout passes, and stops early if ctx is cancelled. A timeout of 0 waits forever.
func (c *Client) BLPop(ctx context.Context, timeout time.Duration, keys ...string) (key, value string, err error) {
	res, err := c.Native.BLPop(ctx, timeout, keys...).Result()
	if err != nil {
		return "", "", notFound(err)
	}
	return res[0], res[1], nil
}

// LRange returns list elements from start to stop inclusive (negative indexes count from the tail).
func (c *Client) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.Native.LRange(ctx, key, start, stop).Result()
}

// LLen returns the length of the list at key.
func (c *Client) LLen(ctx context.Context, key string) (int64, error) {
	return c.Native.LLen(ctx, key).Result()
}

// LTrim keeps only the elements from start to stop inclusive.
func (c *Client) LTrim(ctx context.Context, key string, start, stop int64) error {
	return c.Native.LTrim(ctx, key, start, stop).Err()
}

// LRem removes up to count elements equal to value (count 0 removes all) and
// returns how many were removed.
func (c *Client) LRem(ctx context.Context, key string, count int64, value any) (int64, error) {
	return c.Native.LRem(ctx, key, count, value).Result()
}

// ---- sets ----

// SAdd adds members and returns how many were new.
func (c *Client) SAdd(ctx context.Context, key string, members ...any) (int64, error) {
	return c.Native.SAdd(ctx, key, members...).Result()
}

// SRem removes members and returns how many existed.
func (c *Client) SRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.Native.SRem(ctx, key, members...).Result()
}

// SMembers returns every member of the set at key.
func (c *Client) SMembers(ctx context.Context, key string) ([]string, error) {
	return c.Native.SMembers(ctx, key).Result()
}

// SMembersMap is SMembers returned as a set (a map with empty values), which is
// handy for membership tests.
func (c *Client) SMembersMap(ctx context.Context, key string) (map[string]struct{}, error) {
	return c.Native.SMembersMap(ctx, key).Result()
}

// SIsMember reports whether member is in the set at key.
func (c *Client) SIsMember(ctx context.Context, key string, member any) (bool, error) {
	return c.Native.SIsMember(ctx, key, member).Result()
}

// SCard returns the number of members in the set at key.
func (c *Client) SCard(ctx context.Context, key string) (int64, error) {
	return c.Native.SCard(ctx, key).Result()
}

// SPop removes and returns a random member, or ErrNotFound if the set is empty.
func (c *Client) SPop(ctx context.Context, key string) (string, error) {
	return get(c.Native.SPop(ctx, key).Result())
}

// ---- sorted sets ----

// ZAdd adds or updates members with scores and returns how many were new.
func (c *Client) ZAdd(ctx context.Context, key string, members ...Z) (int64, error) {
	return c.Native.ZAdd(ctx, key, members...).Result()
}

// ZRem removes members and returns how many existed.
func (c *Client) ZRem(ctx context.Context, key string, members ...any) (int64, error) {
	return c.Native.ZRem(ctx, key, members...).Result()
}

// ZRange returns members from rank start to stop inclusive, lowest score first.
func (c *Client) ZRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.Native.ZRange(ctx, key, start, stop).Result()
}

// ZRangeWithScores is ZRange that also returns each member's score.
func (c *Client) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]Z, error) {
	return c.Native.ZRangeWithScores(ctx, key, start, stop).Result()
}

// ZRevRange returns members from rank start to stop inclusive, highest score first.
func (c *Client) ZRevRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.Native.ZRevRange(ctx, key, start, stop).Result()
}

// ZScore returns the score of member, or ErrNotFound.
func (c *Client) ZScore(ctx context.Context, key, member string) (float64, error) {
	return get(c.Native.ZScore(ctx, key, member).Result())
}

// ZRank returns the zero-based rank of member (lowest score first), or ErrNotFound.
func (c *Client) ZRank(ctx context.Context, key, member string) (int64, error) {
	return get(c.Native.ZRank(ctx, key, member).Result())
}

// ZIncrBy adds delta to the score of member and returns the new score.
func (c *Client) ZIncrBy(ctx context.Context, key string, delta float64, member string) (float64, error) {
	return c.Native.ZIncrBy(ctx, key, delta, member).Result()
}

// ZCard returns the number of members in the sorted set at key.
func (c *Client) ZCard(ctx context.Context, key string) (int64, error) {
	return c.Native.ZCard(ctx, key).Result()
}

// ZCount returns the number of members with a score between min and max, each
// given as a string such as "0", "(5" (exclusive), "-inf" or "+inf".
func (c *Client) ZCount(ctx context.Context, key, min, max string) (int64, error) {
	return c.Native.ZCount(ctx, key, min, max).Result()
}

// ---- pub/sub ----

// Publish sends message to channel and returns how many subscribers received it.
func (c *Client) Publish(ctx context.Context, channel string, message any) (int64, error) {
	return c.Native.Publish(ctx, channel, message).Result()
}

// Subscribe subscribes to channels. Read messages with sub.Channel() or
// sub.ReceiveMessage(ctx); the caller must call sub.Close() when finished.
func (c *Client) Subscribe(ctx context.Context, channels ...string) *PubSub {
	return c.Native.Subscribe(ctx, channels...)
}

// ---- pipelines, transactions, scripts ----

// Pipelined queues the commands issued in fn and sends them in one round trip.
// Results are read from the commands fn created, after Pipelined returns.
func (c *Client) Pipelined(ctx context.Context, fn func(Pipeliner) error) error {
	_, err := c.Native.Pipelined(ctx, fn)
	return notFoundUnlessReal(err)
}

// TxPipelined is Pipelined wrapped in MULTI/EXEC, so the commands run atomically.
func (c *Client) TxPipelined(ctx context.Context, fn func(Pipeliner) error) error {
	_, err := c.Native.TxPipelined(ctx, fn)
	return notFoundUnlessReal(err)
}

// Watch runs fn as an optimistic transaction: if any of keys changes before fn's
// MULTI/EXEC runs, the transaction fails with go-redis's TxFailedErr and the
// caller may retry.
func (c *Client) Watch(ctx context.Context, fn func(*Tx) error, keys ...string) error {
	return c.Native.Watch(ctx, fn, keys...)
}

// Eval runs a Lua script atomically on the server. It returns ErrNotFound if the
// script returns nil.
func (c *Client) Eval(ctx context.Context, script string, keys []string, args ...any) (any, error) {
	return get(c.Native.Eval(ctx, script, keys, args...).Result())
}

// notFoundUnlessReal leaves pipeline errors alone: a Nil reply inside a pipeline
// belongs to one command (read it from that command), not to the whole batch.
func notFoundUnlessReal(err error) error {
	if errors.Is(err, goredis.Nil) {
		return nil
	}
	return err
}
