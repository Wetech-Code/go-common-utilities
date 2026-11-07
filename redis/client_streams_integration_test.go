//go:build integration

package redis_test

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Lists, blocking pops, bit operations, geo and streams.
func TestIntegrationListBitGeoStreamCommands(t *testing.T) {
	ctx, c := newClient(t, startServer(t))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	Z := func(s float64, m string) goredis.Z { return goredis.Z{Score: s, Member: m} }
	must(e(c.RPush(ctx, "l", "a", "b", "c", "d", "e")))
	must(e(c.RPush(ctx, "bl", "1", "2", "3", "4", "5", "6")))
	must(e(c.ZAdd(ctx, "bz", Z(1, "a"), Z(2, "b"), Z(3, "c"), Z(4, "d"))))
	must(c.Set(ctx, "str1", "ohmytext", 0))
	must(c.Set(ctx, "str2", "mynewtext", 0))
	must(c.Set(ctx, "bits1", "\xff\x0f", 0))
	must(c.Set(ctx, "bits2", "\x0f\xff", 0))
	must(e(c.GeoAdd(ctx, "geo",
		&goredis.GeoLocation{Name: "palermo", Longitude: 13.361389, Latitude: 38.115556},
		&goredis.GeoLocation{Name: "catania", Longitude: 15.087269, Latitude: 37.502669})))
	around := goredis.GeoSearchQuery{Longitude: 15, Latitude: 37, Radius: 200, RadiusUnit: "km"}
	radius := func(store string) *goredis.GeoRadiusQuery {
		return &goredis.GeoRadiusQuery{Radius: 200, Unit: "km", Store: store}
	}
	for _, id := range []string{"1-1", "2-1", "3-1", "4-1", "5-1"} {
		must(e(c.XAdd(ctx, &goredis.XAddArgs{Stream: "xs", ID: id, Values: map[string]any{"n": id}})))
	}
	must(e(c.XGroupCreateMkStream(ctx, "xs", "g", "0")))
	must(e(c.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: "g", Consumer: "c1", Streams: []string{"xs", ">"}, Count: 5})))

	runSmoke(t, []smoke{
		// lists
		{"LCS", func() error { return e(c.LCS(ctx, &goredis.LCSQuery{Key1: "str1", Key2: "str2", Len: true})) }},
		{"LInsertAfter", func() error { return e(c.LInsertAfter(ctx, "l", "a", "a2")) }},
		{"LInsertBefore", func() error { return e(c.LInsertBefore(ctx, "l", "c", "b2")) }},
		{"LPopCount", func() error { return e(c.LPopCount(ctx, "l", 1)) }},
		{"LPosCount", func() error { return e(c.LPosCount(ctx, "l", "b", 2, goredis.LPosArgs{})) }},
		{"LMPop", func() error {
			_, _, err := c.LMPop(ctx, "LEFT", 1, "l")
			return err
		}},
		{"LMoveM", func() error {
			return e(c.LMoveM(ctx, "l", "lm", "LEFT", "RIGHT", goredis.LMoveMArgs{Count: 2}))
		}},

		// blocking pops (data is already present, so none of them waits)
		{"BLMPop", func() error {
			_, _, err := c.BLMPop(ctx, time.Second, "LEFT", 1, "bl")
			return err
		}},
		{"BLMove", func() error { return e(c.BLMove(ctx, "bl", "bl2", "LEFT", "RIGHT", time.Second)) }},
		{"BLMoveM", func() error {
			return e(c.BLMoveM(ctx, "bl", "bl3", "LEFT", "RIGHT", time.Second, goredis.LMoveMArgs{Count: 1}))
		}},
		{"BRPop", func() error { return e(c.BRPop(ctx, time.Second, "bl")) }},
		{"BRPopLPush", func() error { return e(c.BRPopLPush(ctx, "bl", "bl4", time.Second)) }},
		{"BZMPop", func() error {
			_, _, err := c.BZMPop(ctx, time.Second, "MIN", 1, "bz")
			return err
		}},
		{"BZPopMax", func() error { return e(c.BZPopMax(ctx, time.Second, "bz")) }},
		{"BZPopMin", func() error { return e(c.BZPopMin(ctx, time.Second, "bz")) }},

		// bit operations
		{"BitField", func() error { return e(c.BitField(ctx, "bf", "SET", "u8", "0", 100)) }},
		{"BitFieldRO", func() error { return e(c.BitFieldRO(ctx, "bf", "u8", "0")) }},
		{"BitOpAndOr", func() error { return e(c.BitOpAndOr(ctx, "bo1", "bits1", "bits2")) }},
		{"BitOpDiff", func() error { return e(c.BitOpDiff(ctx, "bo2", "bits1", "bits2")) }},
		{"BitOpDiff1", func() error { return e(c.BitOpDiff1(ctx, "bo3", "bits1", "bits2")) }},
		{"BitOpNot", func() error { return e(c.BitOpNot(ctx, "bo4", "bits1")) }},
		{"BitOpOne", func() error { return e(c.BitOpOne(ctx, "bo5", "bits1", "bits2")) }},
		{"BitOpOr", func() error { return e(c.BitOpOr(ctx, "bo6", "bits1", "bits2")) }},
		{"BitOpXor", func() error { return e(c.BitOpXor(ctx, "bo7", "bits1", "bits2")) }},
		{"BitPosSpan", func() error { return e(c.BitPosSpan(ctx, "bits1", 1, 0, -1, "BYTE")) }},

		// geo
		{"GeoRadius", func() error { return e(c.GeoRadius(ctx, "geo", 15, 37, radius(""))) }},
		{"GeoRadiusByMember", func() error { return e(c.GeoRadiusByMember(ctx, "geo", "palermo", radius(""))) }},
		{"GeoRadiusStore", func() error { return e(c.GeoRadiusStore(ctx, "geo", 15, 37, radius("geodst1"))) }},
		{"GeoRadiusByMemberStore", func() error { return e(c.GeoRadiusByMemberStore(ctx, "geo", "palermo", radius("geodst2"))) }},
		{"GeoSearchLocation", func() error {
			return e(c.GeoSearchLocation(ctx, "geo", &goredis.GeoSearchLocationQuery{GeoSearchQuery: around, WithCoord: true}))
		}},
		{"GeoSearchStore", func() error {
			return e(c.GeoSearchStore(ctx, "geo", "geodst3", &goredis.GeoSearchStoreQuery{GeoSearchQuery: around}))
		}},

		// streams
		{"XRangeN", func() error { return e(c.XRangeN(ctx, "xs", "-", "+", 2)) }},
		{"XRevRange", func() error { return e(c.XRevRange(ctx, "xs", "+", "-")) }},
		{"XRevRangeN", func() error { return e(c.XRevRangeN(ctx, "xs", "+", "-", 2)) }},
		{"XRead", func() error { return e(c.XRead(ctx, &goredis.XReadArgs{Streams: []string{"xs", "0"}, Count: 2})) }},
		{"XReadStreams", func() error { return e(c.XReadStreams(ctx, "xs", "0")) }},
		{"XGroupCreateConsumer", func() error { return e(c.XGroupCreateConsumer(ctx, "xs", "g", "c2")) }},
		{"XInfoConsumers", func() error { return e(c.XInfoConsumers(ctx, "xs", "g")) }},
		{"XInfoStream", func() error { return e(c.XInfoStream(ctx, "xs")) }},
		{"XInfoStreamFull", func() error { return e(c.XInfoStreamFull(ctx, "xs", 10)) }},
		{"XPendingExt", func() error {
			return e(c.XPendingExt(ctx, &goredis.XPendingExtArgs{Stream: "xs", Group: "g", Start: "-", End: "+", Count: 10}))
		}},
		{"XClaim", func() error {
			return e(c.XClaim(ctx, &goredis.XClaimArgs{Stream: "xs", Group: "g", Consumer: "c2", Messages: []string{"1-1"}}))
		}},
		{"XClaimJustID", func() error {
			return e(c.XClaimJustID(ctx, &goredis.XClaimArgs{Stream: "xs", Group: "g", Consumer: "c1", Messages: []string{"1-1"}}))
		}},
		{"XAutoClaim", func() error {
			_, _, err := c.XAutoClaim(ctx, &goredis.XAutoClaimArgs{Stream: "xs", Group: "g", Consumer: "c2", Start: "0", Count: 10})
			return err
		}},
		{"XAutoClaimJustID", func() error {
			_, _, err := c.XAutoClaimJustID(ctx, &goredis.XAutoClaimArgs{Stream: "xs", Group: "g", Consumer: "c1", Start: "0", Count: 10})
			return err
		}},
		{"XAutoClaimWithDeleted", func() error {
			_, _, _, err := c.XAutoClaimWithDeleted(ctx, &goredis.XAutoClaimArgs{Stream: "xs", Group: "g", Consumer: "c1", Start: "0", Count: 10})
			return err
		}},
		{"XNack", func() error {
			return e(c.XNack(ctx, &goredis.XNackArgs{Stream: "xs", Group: "g", Mode: goredis.XNackModeSilent, IDs: []string{"2-1"}}))
		}},
		{"XCfgSet", func() error {
			return e(c.XCfgSet(ctx, &goredis.XCfgSetArgs{Stream: "xs", Duration: 100, MaxSize: 100}))
		}},
		{"XGroupSetID", func() error { return e(c.XGroupSetID(ctx, "xs", "g", "0")) }},
		{"XAckDel", func() error { return e(c.XAckDel(ctx, "xs", "g", "KEEPREF", "3-1")) }},
		{"XDelEx", func() error { return e(c.XDelEx(ctx, "xs", "KEEPREF", "4-1")) }},
		{"XGroupDelConsumer", func() error { return e(c.XGroupDelConsumer(ctx, "xs", "g", "c2")) }},
		{"XTrimMaxLen", func() error { return e(c.XTrimMaxLen(ctx, "xs", 100)) }},
		{"XTrimMaxLenApprox", func() error { return e(c.XTrimMaxLenApprox(ctx, "xs", 100, 0)) }},
		{"XTrimMaxLenMode", func() error { return e(c.XTrimMaxLenMode(ctx, "xs", 100, "KEEPREF")) }},
		{"XTrimMaxLenApproxMode", func() error { return e(c.XTrimMaxLenApproxMode(ctx, "xs", 100, 0, "KEEPREF")) }},
		{"XTrimMinID", func() error { return e(c.XTrimMinID(ctx, "xs", "0")) }},
		{"XTrimMinIDApprox", func() error { return e(c.XTrimMinIDApprox(ctx, "xs", "0", 0)) }},
		{"XTrimMinIDMode", func() error { return e(c.XTrimMinIDMode(ctx, "xs", "0", "KEEPREF")) }},
		{"XTrimMinIDApproxMode", func() error { return e(c.XTrimMinIDApproxMode(ctx, "xs", "0", 0, "KEEPREF")) }},
		{"XGroupDestroy", func() error { return e(c.XGroupDestroy(ctx, "xs", "g")) }},
	})
}
