package memory

import "testing"

func TestAssessDeduplicatesSourcesAndAllowsStrongFactRecovery(t *testing.T) {
	initial := Assess([]WeightedEvidence{{Relation: Supports, IndependenceGroup: "self", Reliability: .95, Directness: .95, Verifiability: .9}})
	if initial.Status != Supported {
		t.Fatalf("initial = %+v", initial)
	}
	contested := Assess([]WeightedEvidence{
		{Relation: Supports, IndependenceGroup: "self", Reliability: .95, Directness: .95, Verifiability: .9},
		{Relation: Contradicts, IndependenceGroup: "syndicated", Reliability: .8, Directness: .8, Verifiability: .9},
		{Relation: Contradicts, IndependenceGroup: "syndicated", Reliability: .8, Directness: .8, Verifiability: .9},
	})
	if contested.IndependentGroups != 2 || contested.ContradictWeight > .58 {
		t.Fatalf("same source was counted repeatedly: %+v", contested)
	}
	recovered := Assess([]WeightedEvidence{
		{Relation: Supports, IndependenceGroup: "self", Reliability: .95, Directness: .95, Verifiability: .9},
		{Relation: Contradicts, IndependenceGroup: "syndicated", Reliability: .8, Directness: .8, Verifiability: .9},
		{Relation: Supports, IndependenceGroup: "primary-record", Reliability: 1, Directness: 1, Verifiability: 1},
	})
	if recovered.Status != Supported || recovered.SupportWeight <= recovered.ContradictWeight {
		t.Fatalf("strong fact did not recover belief: %+v", recovered)
	}
}

func TestTokenizeSupportsChineseAndLatinWithoutPlaintextIndexPolicy(t *testing.T) {
	terms := tokenize("\u6211\u559c\u6b22 AI \u65c5\u884c\u89c4\u5212")
	for _, expected := range []string{"\u6211", "\u559c\u6b22", "ai", "\u65c5\u884c"} {
		found := false
		for _, term := range terms {
			found = found || term == expected
		}
		if !found {
			t.Fatalf("term %q missing from %#v", expected, terms)
		}
	}
}

func TestSecretLikeValuesAreRecognizedWithoutEchoingThem(t *testing.T) {
	if !looksSecret("api_key=example-secret-value") || !looksSecret("\u5bc6\u7801\u662f correct-horse-battery-staple") {
		t.Fatal("secret marker was not recognized")
	}
	if looksSecret("\u7528\u6237\u4e0d\u5e0c\u671b\u628a\u666e\u901a\u8d44\u6599\u53eb\u505a\u5bc6\u7801\u5b66\u6750\u6599") || looksSecret("API key \u4e0d\u5e94\u8be5\u4fdd\u5b58") || looksSecret("\u5bc6\u7801\u6bcf\u6b21\u90fd\u7531\u7528\u6237\u81ea\u5df1\u8f93\u5165") {
		t.Fatal("ordinary sentence was classified as a credential")
	}
}
