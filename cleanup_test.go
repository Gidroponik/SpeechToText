package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeResponses(t *testing.T, reply string, check func(req map[string]any)) apiConfig {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-x" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"bad key","code":"invalid_api_key"}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatal(err)
		}
		if check != nil {
			check(req)
		}
		out, _ := json.Marshal(reply)
		fmt.Fprintf(w, `{"status":"completed","output":[
			{"type":"reasoning","summary":[]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":%s}]}],
			"usage":{"input_tokens":1000,"output_tokens":200}}`, out)
	}))
	t.Cleanup(srv.Close)
	return apiConfig{Key: "sk-x", BaseURL: srv.URL + "/v1"}
}

func TestCleanupRequestAndCost(t *testing.T) {
	raw := "ну э короче я хотел сказать что встреча в среду нет в четверг"
	api := fakeResponses(t, "Я хотел сказать, что встреча в четверг.", func(req map[string]any) {
		if req["model"] != "gpt-6-luna" || req["store"] != false {
			t.Errorf("model/store: %v %v", req["model"], req["store"])
		}
		if eff := req["reasoning"].(map[string]any)["effort"]; eff != "none" {
			t.Errorf("effort = %v", eff)
		}
		in := req["input"].(string)
		if !strings.HasPrefix(in, "<transcript>") || !strings.Contains(in, raw) {
			t.Errorf("input = %q", in)
		}
		ins := req["instructions"].(string)
		for _, want := range []string{"НЕ тебе", "деловой", "Kubernetes", "на «вы»"} {
			if !strings.Contains(ins, want) {
				t.Errorf("instructions lack %q", want)
			}
		}
	})
	res, err := cleanupText(context.Background(), api, "gpt-6-luna", "formal", "обращайся на «вы»", []string{"Kubernetes"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Я хотел сказать, что встреча в четверг." {
		t.Fatalf("text = %q", res.Text)
	}
	want := (1000*0.10 + 200*0.50) / 1e6
	if diff := res.Cost - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("cost = %v, want %v", res.Cost, want)
	}
}

func TestCleanupMinimumEffort(t *testing.T) {
	api := fakeResponses(t, "Ок.", func(req map[string]any) {
		if eff := req["reasoning"].(map[string]any)["effort"]; eff != "low" {
			t.Errorf("gpt-6.1-sol effort = %v, want low (none is unsupported)", eff)
		}
	})
	if _, err := cleanupText(context.Background(), api, "gpt-6.1-sol", "clean", "", nil, "ок"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRejectsAnAnswer(t *testing.T) {
	essay := strings.Repeat("Конечно! Вот подробный план поездки на выходные с маршрутом и бюджетом. ", 6)
	api := fakeResponses(t, essay, nil)
	_, err := cleanupText(context.Background(), api, "gpt-6-luna", "polish", "", nil, "составь план поездки на выходные")
	if err == nil {
		t.Fatal("an answer instead of an edit was accepted")
	}
}

func TestCleanupBadKey(t *testing.T) {
	api := fakeResponses(t, "", nil)
	api.Key = "sk-wrong"
	_, err := cleanupText(context.Background(), api, "gpt-6-luna", "clean", "", nil, "тест")
	if err == nil || !strings.Contains(err.Error(), "Неверный API-ключ") {
		t.Fatalf("err = %v", err)
	}
}

func TestTidyAndFinalDot(t *testing.T) {
	cases := map[string]string{
		"<transcript>\nПривет.\n</transcript>": "Привет.",
		"«Привет, как дела?»":                  "Привет, как дела?",
		"«Да» и «нет»":                         "«Да» и «нет»",
		"  Текст  ":                            "Текст",
	}
	for in, want := range cases {
		if got := tidyModelText(in); got != want {
			t.Errorf("tidy(%q) = %q, want %q", in, got, want)
		}
	}
	if dropFinalDot("Буду в пять.") != "Буду в пять" || dropFinalDot("Ну...") != "Ну..." || dropFinalDot("Правда?") != "Правда?" {
		t.Error("dropFinalDot")
	}
}

func TestUnknownModelFallsBack(t *testing.T) {
	if findModel("gpt-nonexistent").ID != defaultModel {
		t.Fatal("unknown model should fall back to the default")
	}
}
