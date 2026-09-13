package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestQueryParsesVector(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"deployment":"d1"},"value":[1000,"2.5"]}]}}`)),
		}, nil
	})}
	client, err := NewPrometheusClient("http://prometheus.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := client.Query(context.Background(), "up", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].Value != 2.5 || samples[0].Metric["deployment"] != "d1" {
		t.Fatalf("samples = %#v", samples)
	}
}
