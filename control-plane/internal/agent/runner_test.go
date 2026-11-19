package agent

import (
	"strings"
	"testing"
)

func TestParseReply(t *testing.T) {
	ok := map[string]string{
		"plain":           `{"action":"final","result":{"status":"SUCCEEDED","summary":"s","output":{"a":1}}}`,
		"fenced":          "```json\n{\"action\":\"final\",\"result\":{\"status\":\"SUCCEEDED\",\"summary\":\"s\",\"output\":{}}}\n```",
		"failed no summ.": `{"action":"final","result":{"status":"FAILED","error":{"code":"x","message":"not enough info"}}}`,
		"hoisted fields":  `{"action":"final","result":{"status":"SUCCEEDED","summary":"s","output":{}},"confidence":0.4,"open_questions":["q"]}`,
		"tool calls":      `{"action":"tool_calls","tool_calls":[{"name":"run.context","arguments":{}}]}`,
		"no output":       `{"action":"final","result":{"status":"SUCCEEDED","summary":"s"}}`,
	}
	for name, in := range ok {
		rep, err := parseReply(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		switch name {
		case "failed no summ.":
			if rep.Result.Summary != "not enough info" {
				t.Errorf("failed summary not derived from error: %q", rep.Result.Summary)
			}
		case "hoisted fields":
			if rep.Result.Confidence == nil || *rep.Result.Confidence != 0.4 || len(rep.Result.OpenQuestions) != 1 {
				t.Errorf("fields not hoisted: %+v", rep.Result)
			}
		}
	}
	bad := map[string]string{
		"not json":         `hello`,
		"unknown action":   `{"action":"think"}`,
		"no result":        `{"action":"final"}`,
		"bad status":       `{"action":"final","result":{"status":"DONE","summary":"s","output":{}}}`,
		"no summary":       `{"action":"final","result":{"status":"SUCCEEDED","output":{}}}`,
		"failed no reason": `{"action":"final","result":{"status":"FAILED"}}`,
		"confidence range": `{"action":"final","result":{"status":"SUCCEEDED","summary":"s","output":{},"confidence":3}}`,
		"empty tool calls": `{"action":"tool_calls","tool_calls":[]}`,
	}
	for name, in := range bad {
		if _, err := parseReply(in); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if !strings.HasSuffix(excerpt(strings.Repeat("x", 10), 4), "…") {
		t.Error("excerpt")
	}
}
