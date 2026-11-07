//go:build integration

package redis_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func fp32(vals ...float32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// Search and the hybrid (text + vector) query, on the redis:8 container image.
func TestIntegrationSearchCommands(t *testing.T) {
	ctx, c := newClient(t, startModuleServer(t))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e(c.FTCreate(ctx, "idx", &goredis.FTCreateOptions{OnHash: true, Prefix: []any{"doc:"}},
		&goredis.FieldSchema{FieldName: "title", FieldType: goredis.SearchFieldTypeText},
		&goredis.FieldSchema{FieldName: "price", FieldType: goredis.SearchFieldTypeNumeric, Sortable: true},
		&goredis.FieldSchema{FieldName: "color", FieldType: goredis.SearchFieldTypeTag})))
	must(c.HSet(ctx, "doc:1", map[string]any{"title": "hello world", "price": 10, "color": "red"}))
	must(c.HSet(ctx, "doc:2", map[string]any{"title": "hello there", "price": 20, "color": "blue"}))
	must(e(c.FTCreate(ctx, "vidx", &goredis.FTCreateOptions{OnHash: true, Prefix: []any{"vec:"}},
		&goredis.FieldSchema{FieldName: "title", FieldType: goredis.SearchFieldTypeText},
		&goredis.FieldSchema{FieldName: "vec", FieldType: goredis.SearchFieldTypeVector,
			VectorArgs: &goredis.FTVectorArgs{FlatOptions: &goredis.FTFlatOptions{Type: "FLOAT32", Dim: 2, DistanceMetric: "L2"}}})))
	must(c.HSet(ctx, "vec:1", map[string]any{"title": "alpha", "vec": fp32(1, 2)}))
	must(c.HSet(ctx, "vec:2", map[string]any{"title": "beta", "vec": fp32(3, 4)}))

	runSmoke(t, []smoke{
		{"FTInfo", func() error { return e(c.FTInfo(ctx, "idx")) }},
		{"FTSearch", func() error {
			r, err := c.FTSearch(ctx, "idx", "hello")
			if err == nil && r.Total != 2 {
				err = fmt.Errorf("FTSearch total = %d, want 2", r.Total)
			}
			return err
		}},
		{"FTSearchWithArgs", func() error {
			return e(c.FTSearchWithArgs(ctx, "idx", "@color:{red}", &goredis.FTSearchOptions{NoContent: true}))
		}},
		{"FTAggregate", func() error { return e(c.FTAggregate(ctx, "idx", "*")) }},
		{"FTAggregateWithArgs", func() error {
			return e(c.FTAggregateWithArgs(ctx, "idx", "*", &goredis.FTAggregateOptions{Verbatim: true}))
		}},
		{"FTCursorRead", func() error {
			// go-redis does not expose the cursor id of an aggregate, so use an unknown id:
			// the server's "not found" reply proves the command was sent and decoded.
			return okOrErr(e(c.FTCursorRead(ctx, "idx", 12345, 1)), "ursor not found", "No such cursor", "not found", "does not exist")
		}},
		{"FTCursorDel", func() error {
			return okOrErr(e(c.FTCursorDel(ctx, "idx", 12345)), "ursor not found", "No such cursor", "not found", "does not exist")
		}},
		{"FTExplain", func() error { return e(c.FTExplain(ctx, "idx", "hello")) }},
		{"FTExplainWithArgs", func() error {
			return e(c.FTExplainWithArgs(ctx, "idx", "hello", &goredis.FTExplainOptions{Dialect: "2"}))
		}},
		{"FTTagVals", func() error { return e(c.FTTagVals(ctx, "idx", "color")) }},
		{"FTSpellCheck", func() error { return e(c.FTSpellCheck(ctx, "idx", "helo")) }},
		{"FTSpellCheckWithArgs", func() error {
			return e(c.FTSpellCheckWithArgs(ctx, "idx", "helo", &goredis.FTSpellCheckOptions{Distance: 2}))
		}},
		{"FTDictAdd", func() error { return e(c.FTDictAdd(ctx, "dict", "foo", "bar")) }},
		{"FTDictDump", func() error { return e(c.FTDictDump(ctx, "dict")) }},
		{"FTDictDel", func() error { return e(c.FTDictDel(ctx, "dict", "foo")) }},
		{"FTSynUpdate", func() error { return e(c.FTSynUpdate(ctx, "idx", "g1", []any{"hello", "hi"})) }},
		{"FTSynUpdateWithArgs", func() error {
			return e(c.FTSynUpdateWithArgs(ctx, "idx", "g2", &goredis.FTSynUpdateOptions{SkipInitialScan: true}, []any{"world", "earth"}))
		}},
		{"FTSynDump", func() error { return e(c.FTSynDump(ctx, "idx")) }},
		{"FTAliasAdd", func() error { return e(c.FTAliasAdd(ctx, "idx", "alias1")) }},
		{"FTAliasUpdate", func() error { return e(c.FTAliasUpdate(ctx, "idx", "alias1")) }},
		{"FTAliasDel", func() error { return e(c.FTAliasDel(ctx, "alias1")) }},
		{"FTAliasList", func() error { return e(c.FTAliasList(ctx, "idx")) }},
		{"FTAlter", func() error { return e(c.FTAlter(ctx, "idx", true, []any{"extra", "TEXT"})) }},
		{"FTConfigGet", func() error { return e(c.FTConfigGet(ctx, "TIMEOUT")) }},
		{"FTConfigSet", func() error { return e(c.FTConfigSet(ctx, "TIMEOUT", 500)) }},
		{"FTHybrid", func() error {
			return e(c.FTHybrid(ctx, "vidx", "alpha", "vec", &goredis.VectorFP32{Val: fp32(1, 2)}))
		}},
		{"FTHybridWithArgs", func() error {
			return e(c.FTHybridWithArgs(ctx, "vidx", &goredis.FTHybridOptions{
				SearchExpressions: []goredis.FTHybridSearchExpression{{Query: "alpha"}},
				VectorExpressions: []goredis.FTHybridVectorExpression{{VectorField: "vec", VectorData: &goredis.VectorFP32{Val: fp32(1, 2)}}},
			}))
		}},
		{"FTDropIndexWithArgs", func() error {
			return e(c.FTDropIndexWithArgs(ctx, "vidx", &goredis.FTDropIndexOptions{DeleteDocs: true}))
		}},
		{"FTDropIndex", func() error { return e(c.FTDropIndex(ctx, "idx")) }},
	})
}

// Vector sets (VADD and friends).
func TestIntegrationVectorSetCommands(t *testing.T) {
	ctx, c := newClient(t, startModuleServer(t))
	v := func(vals ...float64) goredis.Vector { return &goredis.VectorValues{Val: vals} }

	runSmoke(t, []smoke{
		{"VAdd", func() error { return e(c.VAdd(ctx, "vs", "a", v(1, 2, 3))) }},
		{"VAddWithArgs", func() error {
			return e(c.VAddWithArgs(ctx, "vs", "b", v(2, 3, 4), &goredis.VAddArgs{SetAttr: `{"kind":"x"}`}))
		}},
		{"VAddMore", func() error {
			if _, err := c.VAdd(ctx, "vs", "c", v(3, 4, 5)); err != nil {
				return err
			}
			return e(c.VAdd(ctx, "vs", "d", v(9, 8, 7)))
		}},
		{"VCard", func() error { return e(c.VCard(ctx, "vs")) }},
		{"VDim", func() error { return e(c.VDim(ctx, "vs")) }},
		{"VEmb", func() error { return e(c.VEmb(ctx, "vs", "a", false)) }},
		{"VGetAttr", func() error { return e(c.VGetAttr(ctx, "vs", "b")) }},
		{"VSetAttr", func() error { return e(c.VSetAttr(ctx, "vs", "a", `{"kind":"y"}`)) }},
		{"VClearAttributes", func() error { return e(c.VClearAttributes(ctx, "vs", "a")) }},
		{"VInfo", func() error { return e(c.VInfo(ctx, "vs")) }},
		{"VIsMember", func() error { return e(c.VIsMember(ctx, "vs", "a")) }},
		{"VLinks", func() error { return e(c.VLinks(ctx, "vs", "a")) }},
		{"VLinksWithScores", func() error { return e(c.VLinksWithScores(ctx, "vs", "a")) }},
		{"VRandMember", func() error { return e(c.VRandMember(ctx, "vs")) }},
		{"VRandMemberCount", func() error { return e(c.VRandMemberCount(ctx, "vs", 2)) }},
		{"VRange", func() error { return e(c.VRange(ctx, "vs", "-", "+", 10)) }},
		{"VSim", func() error { return e(c.VSim(ctx, "vs", v(1, 2, 3))) }},
		{"VSimWithScores", func() error { return e(c.VSimWithScores(ctx, "vs", v(1, 2, 3))) }},
		{"VSimWithArgs", func() error { return e(c.VSimWithArgs(ctx, "vs", v(1, 2, 3), &goredis.VSimArgs{Count: 2})) }},
		{"VSimWithArgsWithScores", func() error {
			return e(c.VSimWithArgsWithScores(ctx, "vs", v(1, 2, 3), &goredis.VSimArgs{Count: 2}))
		}},
		{"VSimWithArgsWithAttribs", func() error {
			return e(c.VSimWithArgsWithAttribs(ctx, "vs", v(1, 2, 3), &goredis.VSimArgs{Count: 2}))
		}},
		{"VSimWithArgsWithScoresWithAttribs", func() error {
			return e(c.VSimWithArgsWithScoresWithAttribs(ctx, "vs", v(1, 2, 3), &goredis.VSimArgs{Count: 2}))
		}},
		{"VRem", func() error { return e(c.VRem(ctx, "vs", "d")) }},
	})
}

// The array type (AR* commands).
func TestIntegrationArrayCommands(t *testing.T) {
	ctx, c := newClient(t, startModuleServer(t))

	runSmoke(t, []smoke{
		{"ARSet", func() error { return e(c.ARSet(ctx, "ar", 0, "10", "20", "30")) }},
		{"ARInsert", func() error { return e(c.ARInsert(ctx, "ar", "40", "50")) }},
		{"ARMSet", func() error {
			return e(c.ARMSet(ctx, "ar", goredis.AREntry{Index: 10, Value: "ten"}, goredis.AREntry{Index: 11, Value: "eleven"}))
		}},
		{"ARGet", func() error { return e(c.ARGet(ctx, "ar", 0)) }},
		{"ARMGet", func() error { return e(c.ARMGet(ctx, "ar", 0, 1, 99)) }},
		{"ARGetRange", func() error { return e(c.ARGetRange(ctx, "ar", 0, 4)) }},
		{"ARLen", func() error { return e(c.ARLen(ctx, "ar")) }},
		{"ARCount", func() error { return e(c.ARCount(ctx, "ar")) }},
		{"ARNext", func() error { return e(c.ARNext(ctx, "ar")) }},
		{"ARSeek", func() error { return e(c.ARSeek(ctx, "ar", 3)) }},
		{"ARLastItems", func() error { return e(c.ARLastItems(ctx, "ar", 2, false)) }},
		{"ARInfo", func() error { return e(c.ARInfo(ctx, "ar")) }},
		{"ARInfoFull", func() error { return e(c.ARInfoFull(ctx, "ar")) }},
		{"ARScan", func() error { return e(c.ARScan(ctx, "ar", 0, 20, &goredis.ARScanArgs{Limit: 10})) }},
		{"ARGrep", func() error {
			return e(c.ARGrep(ctx, "ar", "-", "+", &goredis.ARGrepArgs{Predicates: []goredis.ARGrepPredicate{{Type: goredis.ARGrepExact, Value: "ten"}}}))
		}},
		{"ARGrepWithValues", func() error {
			return e(c.ARGrepWithValues(ctx, "ar", "-", "+", &goredis.ARGrepArgs{Predicates: []goredis.ARGrepPredicate{{Type: goredis.ARGrepGlob, Value: "*e*"}}}))
		}},
		{"AROpSum", func() error { return e(c.AROpSum(ctx, "ar", 0, 4)) }},
		{"AROpMin", func() error { return e(c.AROpMin(ctx, "ar", 0, 4)) }},
		{"AROpMax", func() error { return e(c.AROpMax(ctx, "ar", 0, 4)) }},
		{"AROpUsed", func() error { return e(c.AROpUsed(ctx, "ar", 0, 4)) }},
		{"AROpMatch", func() error { return e(c.AROpMatch(ctx, "ar", 0, 4, "10")) }},
		{"AROpAnd", func() error { return e(c.AROpAnd(ctx, "ar", 0, 4)) }},
		{"AROpOr", func() error { return e(c.AROpOr(ctx, "ar", 0, 4)) }},
		{"AROpXor", func() error { return e(c.AROpXor(ctx, "ar", 0, 4)) }},
		{"ARRing", func() error { return e(c.ARRing(ctx, "ring", 3, "a", "b", "c", "d")) }},
		{"ARDel", func() error { return e(c.ARDel(ctx, "ar", 1)) }},
		{"ARDelRange", func() error { return e(c.ARDelRange(ctx, "ar", goredis.ARRange{Start: 10, End: 11})) }},
	})
}
