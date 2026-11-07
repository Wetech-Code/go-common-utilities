//go:build integration

package redis_test

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// Sorted sets, sets, conditional string writes, SORT and hash-field expiry.
func TestIntegrationCollectionCommands(t *testing.T) {
	ctx, c := newClient(t, startServer(t))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	Z := func(s float64, m string) goredis.Z { return goredis.Z{Score: s, Member: m} }
	must(e(c.ZAdd(ctx, "z1", Z(1, "a"), Z(2, "b"), Z(3, "c"))))
	must(e(c.ZAdd(ctx, "z2", Z(2, "b"), Z(3, "c"), Z(4, "d"))))
	must(e(c.ZAdd(ctx, "lex", Z(0, "a"), Z(0, "b"), Z(0, "c"), Z(0, "d"))))
	must(e(c.ZAdd(ctx, "zt", Z(1, "x"), Z(2, "y"), Z(3, "z"))))
	must(e(c.SAdd(ctx, "s1", "a", "b", "c")))
	must(e(c.SAdd(ctx, "s2", "b", "c", "d")))
	must(e(c.RPush(ctx, "nums", "3", "1", "2")))
	must(c.HSet(ctx, "h", map[string]any{"f1": "1", "f2": "2", "f3": "3"}))
	must(c.Set(ctx, "cas", "one", 0))
	both := &goredis.ZStore{Keys: []string{"z1", "z2"}}

	runSmoke(t, []smoke{
		// sorted sets
		{"ZAddArgs", func() error {
			return e(c.ZAddArgs(ctx, "z1", goredis.ZAddArgs{NX: true, Members: []goredis.Z{Z(9, "n")}}))
		}},
		{"ZAddArgsIncr", func() error { return e(c.ZAddArgsIncr(ctx, "z1", goredis.ZAddArgs{Members: []goredis.Z{Z(1, "a")}})) }},
		{"ZAddGT", func() error { return e(c.ZAddGT(ctx, "z1", Z(10, "a"))) }},
		{"ZAddLT", func() error { return e(c.ZAddLT(ctx, "z1", Z(1, "a"))) }},
		{"ZAddNX", func() error { return e(c.ZAddNX(ctx, "z1", Z(5, "m"))) }},
		{"ZAddXX", func() error { return e(c.ZAddXX(ctx, "z1", Z(5, "m"))) }},
		{"ZDiff", func() error { return e(c.ZDiff(ctx, "z1", "z2")) }},
		{"ZDiffStore", func() error { return e(c.ZDiffStore(ctx, "zdiff", "z1", "z2")) }},
		{"ZDiffWithScores", func() error { return e(c.ZDiffWithScores(ctx, "z1", "z2")) }},
		{"ZInter", func() error { return e(c.ZInter(ctx, both)) }},
		{"ZInterWithScores", func() error { return e(c.ZInterWithScores(ctx, both)) }},
		{"ZInterStore", func() error { return e(c.ZInterStore(ctx, "zinter", both)) }},
		{"ZInterCard", func() error { return e(c.ZInterCard(ctx, 0, "z1", "z2")) }},
		{"ZUnion", func() error { return e(c.ZUnion(ctx, *both)) }},
		{"ZUnionWithScores", func() error { return e(c.ZUnionWithScores(ctx, *both)) }},
		{"ZLexCount", func() error { return e(c.ZLexCount(ctx, "lex", "-", "+")) }},
		{"ZRangeByLex", func() error { return e(c.ZRangeByLex(ctx, "lex", &goredis.ZRangeBy{Min: "-", Max: "+"})) }},
		{"ZRevRangeByLex", func() error { return e(c.ZRevRangeByLex(ctx, "lex", &goredis.ZRangeBy{Min: "-", Max: "+"})) }},
		{"ZRemRangeByLex", func() error { return e(c.ZRemRangeByLex(ctx, "lex", "[d", "[d")) }},
		{"ZMPop", func() error {
			_, _, err := c.ZMPop(ctx, "MIN", 1, "z2")
			return err
		}},
		{"ZRandMember", func() error { return e(c.ZRandMember(ctx, "z1", 2)) }},
		{"ZRandMemberWithScores", func() error { return e(c.ZRandMemberWithScores(ctx, "z1", 2)) }},
		{"ZRangeArgs", func() error { return e(c.ZRangeArgs(ctx, goredis.ZRangeArgs{Key: "z1", Start: 0, Stop: -1})) }},
		{"ZRangeArgsWithScores", func() error {
			return e(c.ZRangeArgsWithScores(ctx, goredis.ZRangeArgs{Key: "z1", Start: 0, Stop: -1}))
		}},
		{"ZRangeStore", func() error { return e(c.ZRangeStore(ctx, "zrs", goredis.ZRangeArgs{Key: "z1", Start: 0, Stop: -1})) }},
		{"ZRangeByScoreWithScores", func() error {
			return e(c.ZRangeByScoreWithScores(ctx, "z1", &goredis.ZRangeBy{Min: "-inf", Max: "+inf"}))
		}},
		{"ZRevRangeByScore", func() error { return e(c.ZRevRangeByScore(ctx, "z1", &goredis.ZRangeBy{Min: "-inf", Max: "+inf"})) }},
		{"ZRevRangeByScoreWithScores", func() error {
			return e(c.ZRevRangeByScoreWithScores(ctx, "z1", &goredis.ZRangeBy{Min: "-inf", Max: "+inf"}))
		}},
		{"ZRevRangeWithScores", func() error { return e(c.ZRevRangeWithScores(ctx, "z1", 0, -1)) }},
		{"ZRankWithScore", func() error { return e(c.ZRankWithScore(ctx, "z1", "b")) }},
		{"ZRevRankWithScore", func() error { return e(c.ZRevRankWithScore(ctx, "z1", "b")) }},
		{"ZRemRangeByRank", func() error { return e(c.ZRemRangeByRank(ctx, "zt", 0, 0)) }},
		{"ZRemRangeByScore", func() error { return e(c.ZRemRangeByScore(ctx, "zt", "-inf", "2")) }},
		{"ZScan", func() error { return e3(c.ZScan(ctx, "z1", 0, "*", 10)) }},

		// sets
		{"SDiffStore", func() error { return e(c.SDiffStore(ctx, "sdiff", "s1", "s2")) }},
		{"SUnionStore", func() error { return e(c.SUnionStore(ctx, "sunion", "s1", "s2")) }},
		{"SInterCard", func() error { return e(c.SInterCard(ctx, 0, "s1", "s2")) }},
		{"SDiffCard", func() error { return e(c.SDiffCard(ctx, &goredis.SDiffCardOptions{}, "s1", "s2")) }},
		{"SUnionCard", func() error { return e(c.SUnionCard(ctx, &goredis.SUnionCardOptions{}, "s1", "s2")) }},
		{"SRandMemberN", func() error { return e(c.SRandMemberN(ctx, "s2", 2)) }},
		{"SScan", func() error { return e3(c.SScan(ctx, "s1", 0, "*", 10)) }},
		{"SPopN", func() error { return e(c.SPopN(ctx, "sunion", 2)) }},
		{"ScanType", func() error { return e3(c.ScanType(ctx, 0, "*", 100, "set")) }},

		// SORT
		{"Sort", func() error { return e(c.Sort(ctx, "nums", &goredis.Sort{Order: "ASC"})) }},
		{"SortInterfaces", func() error { return e(c.SortInterfaces(ctx, "nums", &goredis.Sort{Order: "DESC"})) }},
		{"SortRO", func() error { return e(c.SortRO(ctx, "nums", &goredis.Sort{Order: "ASC"})) }},
		{"SortStore", func() error { return e(c.SortStore(ctx, "nums", "sorted", &goredis.Sort{})) }},

		// conditional and compare-and-set strings (Redis 8.4+)
		{"Digest", func() error { return e(c.Digest(ctx, "cas")) }},
		{"SetIFEQ", func() error { return e(c.SetIFEQ(ctx, "cas", "two", "one", 0)) }},
		{"SetIFEQGet", func() error { return e(c.SetIFEQGet(ctx, "cas", "three", "two", 0)) }},
		{"SetIFNE", func() error { return e(c.SetIFNE(ctx, "cas", "four", "zzz", 0)) }},
		{"SetIFNEGet", func() error { return e(c.SetIFNEGet(ctx, "cas", "five", "zzz", 0)) }},
		{"SetIFDEQ", func() error {
			d, err := c.Digest(ctx, "cas")
			if err != nil {
				return err
			}
			return e(c.SetIFDEQ(ctx, "cas", "six", d, 0))
		}},
		{"SetIFDEQGet", func() error {
			d, err := c.Digest(ctx, "cas")
			if err != nil {
				return err
			}
			return e(c.SetIFDEQGet(ctx, "cas", "seven", d, 0))
		}},
		{"SetIFDNE", func() error { return e(c.SetIFDNE(ctx, "cas", "eight", 12345, 0)) }},
		{"SetIFDNEGet", func() error { return e(c.SetIFDNEGet(ctx, "cas", "nine", 12345, 0)) }},
		{"DelExArgs", func() error { return e(c.DelExArgs(ctx, "cas", goredis.DelExArgs{Mode: "IFEQ", MatchValue: "nine"})) }},
		{"MSetEX", func() error {
			return e(c.MSetEX(ctx, goredis.MSetEXArgs{Condition: goredis.NX, Expiration: &goredis.ExpirationOption{Mode: goredis.EX, Value: 100}}, "me1", "1", "me2", "2"))
		}},
		{"IncrEXInt", func() error {
			return e(c.IncrEXInt(ctx, "iex", goredis.IncrEXIntArgs{By: 5, HasBy: true}))
		}},
		{"IncrEXFloat", func() error { return e(c.IncrEXFloat(ctx, "iexf", goredis.IncrEXFloatArgs{By: 1.5})) }},

		// hash field expiry and hash extras
		{"HExpire", func() error { return e(c.HExpire(ctx, "h", time.Hour, "f1")) }},
		{"HExpireAt", func() error { return e(c.HExpireAt(ctx, "h", time.Now().Add(time.Hour), "f1")) }},
		{"HExpireAtWithArgs", func() error {
			return e(c.HExpireAtWithArgs(ctx, "h", time.Now().Add(2*time.Hour), goredis.HExpireArgs{GT: true}, "f1"))
		}},
		{"HExpireWithArgs", func() error { return e(c.HExpireWithArgs(ctx, "h", 3*time.Hour, goredis.HExpireArgs{GT: true}, "f1")) }},
		{"HExpireTime", func() error { return e(c.HExpireTime(ctx, "h", "f1")) }},
		{"HPExpire", func() error { return e(c.HPExpire(ctx, "h", time.Hour, "f2")) }},
		{"HPExpireAt", func() error { return e(c.HPExpireAt(ctx, "h", time.Now().Add(time.Hour), "f2")) }},
		{"HPExpireAtWithArgs", func() error {
			return e(c.HPExpireAtWithArgs(ctx, "h", time.Now().Add(2*time.Hour), goredis.HExpireArgs{GT: true}, "f2"))
		}},
		{"HPExpireWithArgs", func() error { return e(c.HPExpireWithArgs(ctx, "h", 3*time.Hour, goredis.HExpireArgs{GT: true}, "f2")) }},
		{"HPExpireTime", func() error { return e(c.HPExpireTime(ctx, "h", "f2")) }},
		{"HTTL", func() error { return e(c.HTTL(ctx, "h", "f1")) }},
		{"HPTTL", func() error { return e(c.HPTTL(ctx, "h", "f1")) }},
		{"HPersist", func() error { return e(c.HPersist(ctx, "h", "f2")) }},
		{"HGetEX", func() error { return e(c.HGetEX(ctx, "h", "f1")) }},
		{"HGetEXWithArgs", func() error {
			return e(c.HGetEXWithArgs(ctx, "h", &goredis.HGetEXOptions{ExpirationType: goredis.HGetEXExpirationEX, ExpirationVal: 100}, "f1"))
		}},
		{"HSetEX", func() error { return e(c.HSetEX(ctx, "h", "f4", "4")) }},
		{"HSetEXWithArgs", func() error {
			return e(c.HSetEXWithArgs(ctx, "h", &goredis.HSetEXOptions{ExpirationType: goredis.HSetEXExpirationEX, ExpirationVal: 100}, "f5", "5"))
		}},
		{"HGetDel", func() error { return e(c.HGetDel(ctx, "h", "f5")) }},
		{"HRandField", func() error { return e(c.HRandField(ctx, "h", 2)) }},
		{"HRandFieldWithValues", func() error { return e(c.HRandFieldWithValues(ctx, "h", 2)) }},
		{"HScanNoValues", func() error { return e3(c.HScanNoValues(ctx, "h", 0, "*", 10)) }},
		{"HStrLen", func() error { return e(c.HStrLen(ctx, "h", "f1")) }},
	})
}
