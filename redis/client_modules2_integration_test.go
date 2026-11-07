//go:build integration

package redis_test

import (
	"errors"
	"testing"

	"github.com/Wetech-Code/go-common-utilities/redis"
	goredis "github.com/redis/go-redis/v9"
)

// JSON, TimeSeries and the probabilistic structures (Bloom, Cuckoo, Count-Min,
// t-digest, Top-K). They need a server with those modules, so the throwaway
// server is the redis:8 container image.
func TestIntegrationJSONAndTimeSeriesCommands(t *testing.T) {
	ctx, c := newClient(t, startModuleServer(t))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	stop := 2

	must(e(c.JSONSet(ctx, "doc", "$", `{"name":"ann","n":1,"arr":[1,2,3],"s":"str","t":true,"o":{"a":1}}`)))
	must(e(c.TSCreateWithArgs(ctx, "ts1", &goredis.TSOptions{Labels: map[string]string{"sensor": "a"}})))
	must(e(c.TSCreateWithArgs(ctx, "ts2", &goredis.TSOptions{Labels: map[string]string{"sensor": "b"}})))
	must(e(c.TSCreate(ctx, "agg")))
	for i, v := range []float64{1, 2, 3} {
		must(e(c.TSAddWithArgs(ctx, "ts1", 1000*(i+1), v, &goredis.TSOptions{})))
		must(e(c.TSAddWithArgs(ctx, "ts2", 1000*(i+1), v*10, &goredis.TSOptions{})))
	}

	runSmoke(t, []smoke{
		// JSON
		{"JSONArrAppend", func() error { return e(c.JSONArrAppend(ctx, "doc", "$.arr", 4)) }},
		{"JSONArrIndex", func() error { return e(c.JSONArrIndex(ctx, "doc", "$.arr", 2)) }},
		{"JSONArrIndexWithArgs", func() error {
			return e(c.JSONArrIndexWithArgs(ctx, "doc", "$.arr", &goredis.JSONArrIndexArgs{Start: 0}, 2))
		}},
		{"JSONArrInsert", func() error { return e(c.JSONArrInsert(ctx, "doc", "$.arr", 0, 0)) }},
		{"JSONArrLen", func() error { return e(c.JSONArrLen(ctx, "doc", "$.arr")) }},
		{"JSONArrPop", func() error { return e(c.JSONArrPop(ctx, "doc", "$.arr", -1)) }},
		{"JSONArrTrim", func() error {
			// go-redis omits start/stop, so the server refuses; the refusal proves the
			// wrapper sent JSON.ARRTRIM (see the method's documentation).
			return okOrErr(e(c.JSONArrTrim(ctx, "doc", "$.arr")), "wrong number of arguments")
		}},
		{"JSONArrTrimWithArgs", func() error {
			return e(c.JSONArrTrimWithArgs(ctx, "doc", "$.arr", &goredis.JSONArrTrimArgs{Start: 0, Stop: &stop}))
		}},
		{"JSONGetWithArgs", func() error {
			return e(c.JSONGetWithArgs(ctx, "doc", &goredis.JSONGetArgs{Indent: " "}, "$.name"))
		}},
		{"JSONMGet", func() error { return e(c.JSONMGet(ctx, "$.name", "doc")) }},
		{"JSONMSet", func() error { return e(c.JSONMSet(ctx, "doc2", "$", `{"a":1}`)) }},
		{"JSONMSetArgs", func() error {
			return e(c.JSONMSetArgs(ctx, []goredis.JSONSetArgs{{Key: "doc3", Path: "$", Value: `{"b":2}`}}))
		}},
		{"JSONMerge", func() error { return e(c.JSONMerge(ctx, "doc", "$", `{"x":1}`)) }},
		{"JSONNumIncrBy", func() error { return e(c.JSONNumIncrBy(ctx, "doc", "$.n", 2)) }},
		{"JSONObjKeys", func() error { return e(c.JSONObjKeys(ctx, "doc", "$")) }},
		{"JSONObjLen", func() error { return e(c.JSONObjLen(ctx, "doc", "$")) }},
		{"JSONSetMode", func() error { return e(c.JSONSetMode(ctx, "doc", "$.m", "1", "NX")) }},
		{"JSONSetWithArgs", func() error {
			return e(c.JSONSetWithArgs(ctx, "doc", "$.m2", "2", &goredis.JSONSetArgsOptions{Mode: "NX"}))
		}},
		{"JSONStrAppend", func() error { return e(c.JSONStrAppend(ctx, "doc", "$.s", `"x"`)) }},
		{"JSONStrLen", func() error { return e(c.JSONStrLen(ctx, "doc", "$.s")) }},
		{"JSONToggle", func() error { return e(c.JSONToggle(ctx, "doc", "$.t")) }},
		{"JSONType", func() error { return e(c.JSONType(ctx, "doc", "$.n")) }},
		{"JSONClear", func() error { return e(c.JSONClear(ctx, "doc", "$.o")) }},
		{"JSONForget", func() error { return e(c.JSONForget(ctx, "doc", "$.t")) }},

		// TimeSeries
		{"TSAlter", func() error {
			return e(c.TSAlter(ctx, "ts1", &goredis.TSAlterOptions{Labels: map[string]string{"sensor": "a", "unit": "c"}}))
		}},
		{"TSInfo", func() error { return e(c.TSInfo(ctx, "ts1")) }},
		{"TSInfoWithArgs", func() error { return e(c.TSInfoWithArgs(ctx, "ts1", &goredis.TSInfoOptions{Debug: true})) }},
		{"TSGetWithArgs", func() error { return e(c.TSGetWithArgs(ctx, "ts1", &goredis.TSGetOptions{Latest: true})) }},
		{"TSMAdd", func() error {
			return e(c.TSMAdd(ctx, [][]any{{"ts1", 4000, 4.0}, {"ts2", 4000, 40.0}}))
		}},
		{"TSIncrBy", func() error { return e(c.TSIncrBy(ctx, "tsc", 5)) }},
		{"TSIncrByWithArgs", func() error {
			return e(c.TSIncrByWithArgs(ctx, "tsw", 5, &goredis.TSIncrDecrOptions{Timestamp: 5000}))
		}},
		{"TSDecrBy", func() error { return e(c.TSDecrBy(ctx, "tsc", 1)) }},
		{"TSDecrByWithArgs", func() error {
			return e(c.TSDecrByWithArgs(ctx, "tsw", 1, &goredis.TSIncrDecrOptions{Timestamp: 6000}))
		}},
		{"TSCreateRule", func() error { return e(c.TSCreateRule(ctx, "ts1", "agg", goredis.Avg, 1000)) }},
		{"TSDeleteRule", func() error { return e(c.TSDeleteRule(ctx, "ts1", "agg")) }},
		{"TSCreateRuleWithArgs", func() error {
			return e(c.TSCreateRuleWithArgs(ctx, "ts1", "agg", goredis.Sum, 1000, &goredis.TSCreateRuleOptions{}))
		}},
		{"TSRange", func() error { return e(c.TSRange(ctx, "ts1", 0, 10000)) }},
		{"TSRangeWithArgs", func() error { return e(c.TSRangeWithArgs(ctx, "ts1", 0, 10000, &goredis.TSRangeOptions{Count: 2})) }},
		{"TSRevRange", func() error { return e(c.TSRevRange(ctx, "ts1", 0, 10000)) }},
		{"TSRevRangeWithArgs", func() error {
			return e(c.TSRevRangeWithArgs(ctx, "ts1", 0, 10000, &goredis.TSRevRangeOptions{Count: 2}))
		}},
		{"TSMGet", func() error { return e(c.TSMGet(ctx, []string{"sensor=a"})) }},
		{"TSMGetWithArgs", func() error {
			return e(c.TSMGetWithArgs(ctx, []string{"sensor=a"}, &goredis.TSMGetOptions{WithLabels: true}))
		}},
		{"TSMRange", func() error { return e(c.TSMRange(ctx, 0, 10000, []string{"sensor=a"})) }},
		{"TSMRangeWithArgs", func() error {
			return e(c.TSMRangeWithArgs(ctx, 0, 10000, []string{"sensor=a"}, &goredis.TSMRangeOptions{Count: 2}))
		}},
		{"TSMRevRange", func() error { return e(c.TSMRevRange(ctx, 0, 10000, []string{"sensor=a"})) }},
		{"TSMRevRangeWithArgs", func() error {
			return e(c.TSMRevRangeWithArgs(ctx, 0, 10000, []string{"sensor=a"}, &goredis.TSMRevRangeOptions{Count: 2}))
		}},
		{"TSNRange", func() error { return e(c.TSNRange(ctx, []string{"ts1", "ts2"}, 0, 10000)) }},
		{"TSNRangeWithArgs", func() error {
			return e(c.TSNRangeWithArgs(ctx, []string{"ts1", "ts2"}, 0, 10000, &goredis.TSNRangeOptions{Count: 2}))
		}},
		{"TSNRevRange", func() error { return e(c.TSNRevRange(ctx, []string{"ts1", "ts2"}, 0, 10000)) }},
		{"TSNRevRangeWithArgs", func() error {
			return e(c.TSNRevRangeWithArgs(ctx, []string{"ts1", "ts2"}, 0, 10000, &goredis.TSNRevRangeOptions{Count: 2}))
		}},
		{"TSQueryIndex", func() error { return e(c.TSQueryIndex(ctx, []string{"sensor=a"})) }},
		{"TSQueryLabels", func() error { return e(c.TSQueryLabels(ctx, []string{"sensor=a"})) }},
		{"TSQueryLabelValues", func() error { return e(c.TSQueryLabelValues(ctx, "sensor", []string{"sensor=a"})) }},
		{"TSRead", func() error { return e(c.TSRead(ctx, "ts1", 0)) }},
		{"TSReadWithArgs", func() error { return e(c.TSReadWithArgs(ctx, "ts1", 0, &goredis.TSReadOptions{MaxCount: 2})) }},
		{"TSDel", func() error { return e(c.TSDel(ctx, "ts2", 0, 1500)) }},
	})
}

func TestIntegrationProbabilisticCommands(t *testing.T) {
	ctx, c := newClient(t, startModuleServer(t))

	// Dump a filter chunk by chunk and load it into a second key.
	copyBF := func() error {
		iter := int64(0)
		for {
			d, err := c.BFScanDump(ctx, "bf", iter)
			if err != nil {
				return err
			}
			if d.Iter == 0 {
				return nil
			}
			if _, err := c.BFLoadChunk(ctx, "bfcopy", d.Iter, d.Data); err != nil {
				return err
			}
			iter = d.Iter
		}
	}
	copyCF := func() error {
		iter := int64(0)
		for {
			d, err := c.CFScanDump(ctx, "cf", iter)
			if errors.Is(err, redis.ErrNotFound) {
				return nil // the final reply is a nil chunk: the dump is complete
			}
			if err != nil {
				return err
			}
			if d.Iter == 0 {
				return nil
			}
			if _, err := c.CFLoadChunk(ctx, "cfcopy", d.Iter, d.Data); err != nil {
				return err
			}
			iter = d.Iter
		}
	}

	runSmoke(t, []smoke{
		// Bloom filter
		{"BFReserveWithArgs", func() error {
			return e(c.BFReserveWithArgs(ctx, "bf", &goredis.BFReserveOptions{Capacity: 100, Error: 0.01}))
		}},
		{"BFReserveExpansion", func() error { return e(c.BFReserveExpansion(ctx, "bfe", 0.01, 100, 2)) }},
		{"BFReserveNonScaling", func() error { return e(c.BFReserveNonScaling(ctx, "bfn", 0.01, 100)) }},
		{"BFMAdd", func() error { return e(c.BFMAdd(ctx, "bf", "a", "b", "c")) }},
		{"BFInsert", func() error { return e(c.BFInsert(ctx, "bf", &goredis.BFInsertOptions{Capacity: 100}, "d", "e")) }},
		{"BFMExists", func() error { return e(c.BFMExists(ctx, "bf", "a", "zz")) }},
		{"BFCard", func() error { return e(c.BFCard(ctx, "bf")) }},
		{"BFInfo", func() error { return e(c.BFInfo(ctx, "bf")) }},
		{"BFInfoArg", func() error { return e(c.BFInfoArg(ctx, "bf", "CAPACITY")) }},
		{"BFInfoCapacity", func() error { return e(c.BFInfoCapacity(ctx, "bf")) }},
		{"BFInfoExpansion", func() error { return e(c.BFInfoExpansion(ctx, "bf")) }},
		{"BFInfoFilters", func() error { return e(c.BFInfoFilters(ctx, "bf")) }},
		{"BFInfoItems", func() error { return e(c.BFInfoItems(ctx, "bf")) }},
		{"BFInfoSize", func() error { return e(c.BFInfoSize(ctx, "bf")) }},
		{"BFScanDump", copyBF},
		{"BFLoadChunk", func() error { return e(c.BFExists(ctx, "bfcopy", "a")) }},

		// Cuckoo filter
		{"CFReserveWithArgs", func() error {
			return e(c.CFReserveWithArgs(ctx, "cf", &goredis.CFReserveOptions{Capacity: 1000, BucketSize: 2}))
		}},
		{"CFReserve", func() error { return e(c.CFReserve(ctx, "cf2", 1000)) }},
		{"CFReserveBucketSize", func() error { return e(c.CFReserveBucketSize(ctx, "cf3", 1000, 4)) }},
		{"CFReserveExpansion", func() error { return e(c.CFReserveExpansion(ctx, "cf4", 1000, 2)) }},
		{"CFReserveMaxIterations", func() error { return e(c.CFReserveMaxIterations(ctx, "cf5", 1000, 20)) }},
		{"CFAdd", func() error { return e(c.CFAdd(ctx, "cf", "a")) }},
		{"CFAddNX", func() error { return e(c.CFAddNX(ctx, "cf", "b")) }},
		{"CFInsert", func() error { return e(c.CFInsert(ctx, "cf", &goredis.CFInsertOptions{Capacity: 1000}, "c", "d")) }},
		{"CFInsertNX", func() error { return e(c.CFInsertNX(ctx, "cf", &goredis.CFInsertOptions{Capacity: 1000}, "e")) }},
		{"CFCount", func() error { return e(c.CFCount(ctx, "cf", "a")) }},
		{"CFExists", func() error { return e(c.CFExists(ctx, "cf", "a")) }},
		{"CFMExists", func() error { return e(c.CFMExists(ctx, "cf", "a", "zz")) }},
		{"CFInfo", func() error { return e(c.CFInfo(ctx, "cf")) }},
		{"CFScanDump", copyCF},
		{"CFLoadChunk", func() error { return e(c.CFExists(ctx, "cfcopy", "a")) }},
		{"CFDel", func() error { return e(c.CFDel(ctx, "cf", "a")) }},

		// Count-Min sketch
		{"CMSInitByProb", func() error { return e(c.CMSInitByProb(ctx, "cms1", 0.01, 0.01)) }},
		{"CMSIncrBy", func() error { return e(c.CMSIncrBy(ctx, "cms1", "a", 3, "b", 2)) }},
		{"CMSQuery", func() error { return e(c.CMSQuery(ctx, "cms1", "a", "b")) }},
		{"CMSInfo", func() error { return e(c.CMSInfo(ctx, "cms1")) }},
		{"CMSMerge", func() error {
			if _, err := c.CMSInitByProb(ctx, "cms2", 0.01, 0.01); err != nil {
				return err
			}
			return e(c.CMSMerge(ctx, "cms2", "cms1"))
		}},
		{"CMSMergeWithWeight", func() error {
			if _, err := c.CMSInitByProb(ctx, "cms3", 0.01, 0.01); err != nil {
				return err
			}
			return e(c.CMSMergeWithWeight(ctx, "cms3", map[string]int64{"cms1": 2}))
		}},

		// t-digest
		{"TDigestCreateWithCompression", func() error { return e(c.TDigestCreateWithCompression(ctx, "td", 100)) }},
		{"TDigestAdd", func() error { return e(c.TDigestAdd(ctx, "td", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)) }},
		{"TDigestMin", func() error { return e(c.TDigestMin(ctx, "td")) }},
		{"TDigestMax", func() error { return e(c.TDigestMax(ctx, "td")) }},
		{"TDigestCDF", func() error { return e(c.TDigestCDF(ctx, "td", 5)) }},
		{"TDigestQuantile", func() error { return e(c.TDigestQuantile(ctx, "td", 0.5)) }},
		{"TDigestRank", func() error { return e(c.TDigestRank(ctx, "td", 5)) }},
		{"TDigestRevRank", func() error { return e(c.TDigestRevRank(ctx, "td", 5)) }},
		{"TDigestByRank", func() error { return e(c.TDigestByRank(ctx, "td", 1)) }},
		{"TDigestByRevRank", func() error { return e(c.TDigestByRevRank(ctx, "td", 1)) }},
		{"TDigestTrimmedMean", func() error { return e(c.TDigestTrimmedMean(ctx, "td", 0.1, 0.9)) }},
		{"TDigestInfo", func() error { return e(c.TDigestInfo(ctx, "td")) }},
		{"TDigestMerge", func() error {
			if _, err := c.TDigestCreate(ctx, "td2"); err != nil {
				return err
			}
			return e(c.TDigestMerge(ctx, "td2", &goredis.TDigestMergeOptions{Compression: 100}, "td"))
		}},
		{"TDigestReset", func() error { return e(c.TDigestReset(ctx, "td")) }},

		// Top-K
		{"TopKReserveWithOptions", func() error { return e(c.TopKReserveWithOptions(ctx, "topk", 3, 50, 7, 0.9)) }},
		{"TopKAdd", func() error { return e(c.TopKAdd(ctx, "topk", "a", "b", "c")) }},
		{"TopKIncrBy", func() error { return e(c.TopKIncrBy(ctx, "topk", "a", 5)) }},
		{"TopKCount", func() error { return e(c.TopKCount(ctx, "topk", "a")) }},
		{"TopKQuery", func() error { return e(c.TopKQuery(ctx, "topk", "a", "zz")) }},
		{"TopKList", func() error { return e(c.TopKList(ctx, "topk")) }},
		{"TopKListWithCount", func() error { return e(c.TopKListWithCount(ctx, "topk")) }},
		{"TopKInfo", func() error { return e(c.TopKInfo(ctx, "topk")) }},
	})
}
