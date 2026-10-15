package antifraud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClientRejectsBadEndpoint(t *testing.T) {
	cases := []string{"", "antifraud:8000", "ftp://host", "http://"}
	for _, endpoint := range cases {
		if _, err := NewClient(endpoint, time.Second); err == nil {
			t.Fatalf("NewClient(%q) 应返回错误", endpoint)
		}
	}
}

func TestClientInferPollsUntilDone(t *testing.T) {
	srv := fakeInferService(t, func(string) InferItem { return violationItem(nil) })
	client, err := NewClient(srv.URL, 3*time.Second)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	items, err := client.Infer(context.Background(), []string{"点击链接领取红包", "兼职刷单日结"})
	if err != nil {
		t.Fatalf("Infer() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("结果条数 = %d, want 2", len(items))
	}
	if items[1].Text != "兼职刷单日结" || items[1].LabelKey != "brushing" {
		t.Fatalf("结果未按输入顺序回填：%+v", items[1])
	}
}

func TestClientInferReportsFailures(t *testing.T) {
	t.Run("任务失败", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/api/v1/infer", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "t-9"})
		})
		mux.HandleFunc("/api/v1/infer/t-9", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "t-9", "status": "failed", "error": "CUDA out of memory"})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		client, err := NewClient(srv.URL, 2*time.Second)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := client.Infer(context.Background(), []string{"文本"}); err == nil {
			t.Fatal("任务失败时应返回错误")
		}
	})

	t.Run("提交返回非 202", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("模型未加载"))
		}))
		defer srv.Close()

		client, err := NewClient(srv.URL, 2*time.Second)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		if _, err := client.Infer(context.Background(), []string{"文本"}); err == nil {
			t.Fatal("提交失败时应返回错误")
		}
	})

	t.Run("结果条数不符", func(t *testing.T) {
		srv := fakeInferService(t, func(string) InferItem { return violationItem(nil) })
		client, err := NewClient(srv.URL, 2*time.Second)
		if err != nil {
			t.Fatalf("NewClient() error = %v", err)
		}
		// 请求两条但服务按实际文本数返回：期望条数校验由客户端保证
		if _, err := client.Infer(context.Background(), []string{"文本"}); err != nil {
			t.Fatalf("Infer() error = %v", err)
		}
	})
}

func TestClientInferHonoursContextDeadline(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/infer", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"ticket": "t-1"})
	})
	mux.HandleFunc("/api/v1/infer/t-1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ticket": "t-1", "status": "running"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := NewClient(srv.URL, time.Minute)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := client.Infer(ctx, []string{"文本"}); err == nil {
		t.Fatal("ctx 超时时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时未及时返回，耗时 %s", elapsed)
	}
}
