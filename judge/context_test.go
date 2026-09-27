package judge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alam0rt/mergegate"
)

func TestAskSendsStateAndNouls(t *testing.T) {
	var req map[string]any
	srv := fakeServer(t, 200, `{"model":"m","answers":{"watch_a":{"type":"noul","noul":0.7}},"usage":{"input_tokens":1,"output_tokens":1}}`, &req)
	got, err := New("test-key", WithServerURL(srv.URL)).Ask(context.Background(),
		map[string]any{"history": []any{"Revert x"}}, map[string]string{"a": "Was it reverted?"})
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != 0.7 {
		t.Errorf("Ask = %v", got)
	}
	qs := req["questions"].(map[string]any)
	if len(qs) != 1 || qs["watch_a"].(map[string]any)["instructions"] != "Was it reverted?" {
		t.Errorf("questions = %v; Ask must send only the questions it was given", qs)
	}
	if s, _ := json.Marshal(req["state"]); !strings.Contains(string(s), "Revert x") {
		t.Errorf("state = %s", s)
	}
}

func TestAskMissingAnswer(t *testing.T) {
	srv := fakeServer(t, 200, `{"model":"m","answers":{},"usage":{"input_tokens":1,"output_tokens":1}}`, nil)
	if _, err := New("test-key", WithServerURL(srv.URL)).Ask(context.Background(), map[string]any{}, map[string]string{"a": "q"}); err == nil {
		t.Error("want an error for a missing answer")
	}
}

func TestContextState(t *testing.T) {
	long := strings.Repeat("x", maxCommentChars+50)
	s := ContextState(bumpPR,
		[]mergegate.Comment{{Author: "sam", Association: "OWNER", Kind: "comment", Body: long, CreatedAt: time.Unix(0, 0)}},
		[]mergegate.Commit{{SHA: "0123456789abcdef", Message: "Revert \"bump\"", Date: time.Unix(0, 0)}},
	)
	if s["files"] == nil {
		t.Error("context calls still carry the diff, so questions can relate the two")
	}
	cs := s["comments"].([]any)
	c0 := cs[0].(map[string]any)
	if len(c0["body"].(string)) != maxCommentChars || c0["truncated"] != true {
		t.Errorf("long comment should be cut to %d chars and marked", maxCommentChars)
	}
	h := s["history"].([]any)[0].(map[string]any)
	if h["sha"] != "0123456789ab" || h["message"] != "Revert \"bump\"" {
		t.Errorf("history entry = %v", h)
	}

	if _, ok := ContextState(bumpPR, nil, []mergegate.Commit{{SHA: "a"}})["comments"]; ok {
		t.Error("an unused source should be absent, not empty")
	}
}
