package usage

import "testing"

func TestMeasurementPresenceSurvivesAccountingAndMerge(t *testing.T) {
 start := &TokenUsageFields{InputTokens:true,CacheReadTokens:true}
 update := &TokenUsageFields{OutputTokens:true}
 merged := MergeTokenUsageFields(start,update)
 if !merged.InputTokens || !merged.OutputTokens || !merged.CacheReadTokens || merged.TotalTokens {t.Fatalf("merged fields = %+v",merged)}
 if start.OutputTokens || update.InputTokens {t.Fatal("merge mutated observations already held by a record")}
 if MergeTokenUsageFields(nil,nil)!=nil {t.Fatal("merge invented observations")}
 for _,fields:=range []*TokenUsageFields{nil,merged} {
  detail:=EnsureTokenBreakdownForProvider(Detail{UsageFields:fields},"openai","")
  if detail.TokenBreakdown.Quality!=TokenAccountingQualityComplete {t.Fatal("zero arithmetic changed")}
  if detail.UsageFields.Reported()!=(fields!=nil) {t.Fatal("arithmetic changed measurement presence")}
 }
}
