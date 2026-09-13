// Package metrics provides bounded clients for platform telemetry. Callers
// provide pre-defined PromQL; user-provided PromQL is never accepted.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

type Sample struct {
	Metric    map[string]string
	Timestamp time.Time
	Value     float64
}

type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

func NewPrometheusClient(rawURL string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Prometheus URL %q", rawURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: parsed, httpClient: httpClient}, nil
}

func (c *Client) Query(ctx context.Context, promQL string, at time.Time) ([]Sample, error) {
	values := url.Values{"query": {promQL}}
	if !at.IsZero() {
		values.Set("time", formatTime(at))
	}
	return c.request(ctx, "/api/v1/query", values)
}

func (c *Client) QueryRange(ctx context.Context, promQL string, start, end time.Time, step time.Duration) ([]Sample, error) {
	if !end.After(start) || step <= 0 {
		return nil, errors.New("invalid query range")
	}
	values := url.Values{
		"query": {promQL},
		"start": {formatTime(start)},
		"end":   {formatTime(end)},
		"step":  {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)},
	}
	return c.request(ctx, "/api/v1/query_range", values)
}

func Fresh(samples []Sample, now time.Time, maximumAge time.Duration) bool {
	for _, sample := range samples {
		if !sample.Timestamp.IsZero() && now.Sub(sample.Timestamp) <= maximumAge {
			return true
		}
	}
	return false
}

func (c *Client) request(ctx context.Context, path string, values url.Values) ([]Sample, error) {
	endpoint := *c.baseURL
	endpoint.Path = path
	endpoint.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query Prometheus: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s", response.Status)
	}
	var envelope struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Value  []json.RawMessage   `json:"value"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if envelope.Status != "success" {
		return nil, fmt.Errorf("Prometheus query failed: %s", envelope.Error)
	}
	var samples []Sample
	for _, result := range envelope.Data.Result {
		if len(result.Value) == 2 {
			sample, err := parseSample(result.Metric, result.Value)
			if err != nil {
				return nil, err
			}
			samples = append(samples, sample)
		}
		for _, value := range result.Values {
			sample, err := parseSample(result.Metric, value)
			if err != nil {
				return nil, err
			}
			samples = append(samples, sample)
		}
	}
	return samples, nil
}

func parseSample(metric map[string]string, raw []json.RawMessage) (Sample, error) {
	if len(raw) != 2 {
		return Sample{}, errors.New("invalid Prometheus sample")
	}
	var timestamp float64
	var encoded string
	if err := json.Unmarshal(raw[0], &timestamp); err != nil {
		return Sample{}, err
	}
	if err := json.Unmarshal(raw[1], &encoded); err != nil {
		return Sample{}, err
	}
	value, err := strconv.ParseFloat(encoded, 64)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Metric: metric, Timestamp: time.Unix(0, int64(timestamp*float64(time.Second))).UTC(), Value: value}, nil
}

func formatTime(value time.Time) string {
	return strconv.FormatFloat(float64(value.UnixNano())/float64(time.Second), 'f', 3, 64)
}
