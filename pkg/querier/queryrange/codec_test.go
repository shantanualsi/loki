package queryrange

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	strings "strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/stretchr/testify/require"

	"github.com/grafana/loki/pkg/loghttp"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/querier/queryrange/queryrangebase"
	"github.com/grafana/loki/pkg/util"
)

func init() {
	time.Local = nil // for easier tests comparison
}

var (
	start = testTime //  Marshalling the time drops the monotonic clock so we can't use time.Now
	end   = start.Add(1 * time.Hour)
)

func Test_codec_EncodeDecodeRequest(t *testing.T) {
	tests := []struct {
		name       string
		reqBuilder func() (*http.Request, error)
		want       queryrangebase.Request
		wantErr    bool
	}{
		{"wrong", func() (*http.Request, error) { return http.NewRequest(http.MethodGet, "/bad?step=bad", nil) }, nil, true},
		{"query_range", func() (*http.Request, error) {
			return http.NewRequest(http.MethodGet,
				fmt.Sprintf(`/query_range?start=%d&end=%d&query={foo="bar"}&step=10&limit=200&direction=FORWARD`, start.UnixNano(), end.UnixNano()), nil)
		}, &LokiRequest{
			Query:     `{foo="bar"}`,
			Limit:     200,
			Step:      10000, // step is expected in ms
			Direction: logproto.FORWARD,
			Path:      "/query_range",
			StartTs:   start,
			EndTs:     end,
		}, false},
		{"query_range", func() (*http.Request, error) {
			return http.NewRequest(http.MethodGet,
				fmt.Sprintf(`/query_range?start=%d&end=%d&query={foo="bar"}&interval=10&limit=200&direction=BACKWARD`, start.UnixNano(), end.UnixNano()), nil)
		}, &LokiRequest{
			Query:     `{foo="bar"}`,
			Limit:     200,
			Step:      14000, // step is expected in ms; calculated default if request param not present
			Interval:  10000, // interval is expected in ms
			Direction: logproto.BACKWARD,
			Path:      "/query_range",
			StartTs:   start,
			EndTs:     end,
		}, false},
		{"series", func() (*http.Request, error) {
			return http.NewRequest(http.MethodGet,
				fmt.Sprintf(`/series?start=%d&end=%d&match={foo="bar"}`, start.UnixNano(), end.UnixNano()), nil)
		}, &LokiSeriesRequest{
			Match:   []string{`{foo="bar"}`},
			Path:    "/series",
			StartTs: start,
			EndTs:   end,
		}, false},
		{"labels", func() (*http.Request, error) {
			return http.NewRequest(http.MethodGet,
				fmt.Sprintf(`/label?start=%d&end=%d`, start.UnixNano(), end.UnixNano()), nil)
		}, &LokiLabelNamesRequest{
			Path:    "/label",
			StartTs: start,
			EndTs:   end,
		}, false},
		{"label_values", func() (*http.Request, error) {
			return http.NewRequest(http.MethodGet,
				fmt.Sprintf(`/label/test/values?start=%d&end=%d&query={foo="bar"}`, start.UnixNano(), end.UnixNano()), nil)
		}, &LokiLabelNamesRequest{
			Path:    "/label/test/values",
			StartTs: start,
			EndTs:   end,
			Query:   `{foo="bar"}`,
		}, false},
		{"index_stats", func() (*http.Request, error) {
			return DefaultCodec.EncodeRequest(context.Background(), &logproto.IndexStatsRequest{
				From:     model.TimeFromUnixNano(start.UnixNano()),
				Through:  model.TimeFromUnixNano(end.UnixNano()),
				Matchers: `{job="foo"}`,
			})
		}, &logproto.IndexStatsRequest{
			From:     model.TimeFromUnixNano(start.UnixNano()),
			Through:  model.TimeFromUnixNano(end.UnixNano()),
			Matchers: `{job="foo"}`,
		}, false},
		{"volume", func() (*http.Request, error) {
			return DefaultCodec.EncodeRequest(context.Background(), &logproto.VolumeRequest{
				From:         model.TimeFromUnixNano(start.UnixNano()),
				Through:      model.TimeFromUnixNano(end.UnixNano()),
				Matchers:     `{job="foo"}`,
				Limit:        3,
				Step:         0,
				TargetLabels: []string{"job"},
				AggregateBy:  "labels",
			})
		}, &logproto.VolumeRequest{
			From:         model.TimeFromUnixNano(start.UnixNano()),
			Through:      model.TimeFromUnixNano(end.UnixNano()),
			Matchers:     `{job="foo"}`,
			Limit:        3,
			Step:         0,
			TargetLabels: []string{"job"},
			AggregateBy:  "labels",
		}, false},
		{"volume_default_limit", func() (*http.Request, error) {
			return DefaultCodec.EncodeRequest(context.Background(), &logproto.VolumeRequest{
				From:     model.TimeFromUnixNano(start.UnixNano()),
				Through:  model.TimeFromUnixNano(end.UnixNano()),
				Matchers: `{job="foo"}`,
			})
		}, &logproto.VolumeRequest{
			From:        model.TimeFromUnixNano(start.UnixNano()),
			Through:     model.TimeFromUnixNano(end.UnixNano()),
			Matchers:    `{job="foo"}`,
			Limit:       100,
			Step:        0,
			AggregateBy: "series",
		}, false},
		{"volume_range", func() (*http.Request, error) {
			return DefaultCodec.EncodeRequest(context.Background(), &logproto.VolumeRequest{
				From:         model.TimeFromUnixNano(start.UnixNano()),
				Through:      model.TimeFromUnixNano(end.UnixNano()),
				Matchers:     `{job="foo"}`,
				Limit:        3,
				Step:         30 * 1e3,
				TargetLabels: []string{"fizz", "buzz"},
			})
		}, &logproto.VolumeRequest{
			From:         model.TimeFromUnixNano(start.UnixNano()),
			Through:      model.TimeFromUnixNano(end.UnixNano()),
			Matchers:     `{job="foo"}`,
			Limit:        3,
			Step:         30 * 1e3, // step is expected in ms
			TargetLabels: []string{"fizz", "buzz"},
			AggregateBy:  "series",
		}, false},
		{"volume_range_default_limit", func() (*http.Request, error) {
			return DefaultCodec.EncodeRequest(context.Background(), &logproto.VolumeRequest{
				From:     model.TimeFromUnixNano(start.UnixNano()),
				Through:  model.TimeFromUnixNano(end.UnixNano()),
				Matchers: `{job="foo"}`,
				Step:     30 * 1e3, // step is expected in ms
			})
		}, &logproto.VolumeRequest{
			From:        model.TimeFromUnixNano(start.UnixNano()),
			Through:     model.TimeFromUnixNano(end.UnixNano()),
			Matchers:    `{job="foo"}`,
			Limit:       100,
			Step:        30 * 1e3, // step is expected in ms; default is 0 or no step
			AggregateBy: "series",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := tt.reqBuilder()
			if err != nil {
				t.Fatal(err)
			}
			got, err := DefaultCodec.DecodeRequest(context.TODO(), req, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("codec.DecodeRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func Test_codec_DecodeResponse(t *testing.T) {
	tests := []struct {
		name    string
		res     *http.Response
		req     queryrangebase.Request
		want    queryrangebase.Response
		wantErr bool
	}{
		{"500", &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("some error"))}, nil, nil, true},
		{"no body", &http.Response{StatusCode: 200, Body: io.NopCloser(badReader{})}, nil, nil, true},
		{"bad json", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil, nil, true},
		{"not success", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"fail"}`))}, nil, nil, true},
		{"unknown", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success"}`))}, nil, nil, true},
		{
			"matrix", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(matrixString))}, nil,
			&LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: loghttp.QueryStatusSuccess,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeMatrix,
						Result:     sampleStreams,
					},
				},
				Statistics: statsResult,
			}, false,
		},
		{
			"matrix-empty-streams",
			&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(matrixStringEmptyResult))},
			nil,
			&LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: loghttp.QueryStatusSuccess,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeMatrix,
						Result:     make([]queryrangebase.SampleStream, 0), // shouldn't be nil.
					},
				},
				Statistics: statsResult,
			}, false,
		},
		{
			"vector-empty-streams",
			&http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(vectorStringEmptyResult))},
			nil,
			&LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: loghttp.QueryStatusSuccess,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeVector,
						Result:     make([]queryrangebase.SampleStream, 0), // shouldn't be nil.
					},
				},
				Statistics: statsResult,
			}, false,
		},
		{
			"streams v1", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(streamsString))},
			&LokiRequest{Direction: logproto.FORWARD, Limit: 100, Path: "/loki/api/v1/query_range"},
			&LokiResponse{
				Status:    loghttp.QueryStatusSuccess,
				Direction: logproto.FORWARD,
				Limit:     100,
				Version:   uint32(loghttp.VersionV1),
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result:     logStreams,
				},
				Statistics: statsResult,
			}, false,
		},
		{
			"streams legacy", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(streamsString))},
			&LokiRequest{Direction: logproto.FORWARD, Limit: 100, Path: "/api/prom/query_range"},
			&LokiResponse{
				Status:    loghttp.QueryStatusSuccess,
				Direction: logproto.FORWARD,
				Limit:     100,
				Version:   uint32(loghttp.VersionLegacy),
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result:     logStreams,
				},
				Statistics: statsResult,
			}, false,
		},
		{
			"series", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(seriesString))},
			&LokiSeriesRequest{Path: "/loki/api/v1/series"},
			&LokiSeriesResponse{
				Status:  "success",
				Version: uint32(loghttp.VersionV1),
				Data:    seriesData,
			}, false,
		},
		{
			"labels legacy", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(labelsString))},
			&LokiLabelNamesRequest{Path: "/api/prom/label"},
			&LokiLabelNamesResponse{
				Status:  "success",
				Version: uint32(loghttp.VersionLegacy),
				Data:    labelsData,
			}, false,
		},
		{
			"index stats", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(indexStatsString))},
			&logproto.IndexStatsRequest{},
			&IndexStatsResponse{
				Response: &logproto.IndexStatsResponse{
					Streams: 1,
					Chunks:  2,
					Bytes:   3,
					Entries: 4,
				},
			}, false,
		},
		{
			"volume", &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(seriesVolumeString))},
			&logproto.VolumeRequest{},
			&VolumeResponse{
				Response: &logproto.VolumeResponse{
					Volumes: []logproto.Volume{
						{Name: `{foo="bar"}`, Volume: 38},
					},
					Limit: 100,
				},
			}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DefaultCodec.DecodeResponse(context.TODO(), tt.res, tt.req)
			if (err != nil) != tt.wantErr {
				t.Errorf("codec.DecodeResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func Test_codec_DecodeProtobufResponseParity(t *testing.T) {
	// test fixtures from pkg/util/marshal_test
	var queryTests = []struct {
		actual   parser.Value
		expected string
	}{
		{
			logqlmodel.Streams{
				logproto.Stream{
					Entries: []logproto.Entry{
						{
							Timestamp: time.Unix(0, 123456789012345),
							Line:      "super line",
						},
					},
					Labels: `{test="test"}`,
				},
			},
			`{
				"status": "success",
				"data": {
					` + statsResultString + `
					"resultType": "streams",
					"result": [
						{
							"stream": {
								"test": "test"
							},
							"values":[
								[ "123456789012345", "super line" ]
							]
						}
					]
				}
			}`,
		},
		// vector test
		{
			promql.Vector{
				{
					T: 1568404331324,
					F: 0.013333333333333334,
					Metric: []labels.Label{
						{
							Name:  "filename",
							Value: `/var/hostlog/apport.log`,
						},
						{
							Name:  "job",
							Value: "varlogs",
						},
					},
				},
				{
					T: 1568404331324,
					F: 3.45,
					Metric: []labels.Label{
						{
							Name:  "filename",
							Value: `/var/hostlog/syslog`,
						},
						{
							Name:  "job",
							Value: "varlogs",
						},
					},
				},
			},
			`{
				"data": {
					` + statsResultString + `
					"resultType": "vector",
					"result": [
						{
						"metric": {
							"filename": "\/var\/hostlog\/apport.log",
							"job": "varlogs"
						},
						"value": [
							1568404331.324,
							"0.013333333333333334"
						]
						},
						{
						"metric": {
							"filename": "\/var\/hostlog\/syslog",
							"job": "varlogs"
						},
						"value": [
							1568404331.324,
							"3.45"
							]
						}
					]
				},
				"status": "success"
			}`,
		},
		// matrix test
		{
			promql.Matrix{
				{
					Floats: []promql.FPoint{
						{
							T: 1568404331324,
							F: 0.013333333333333334,
						},
					},
					Metric: []labels.Label{
						{
							Name:  "filename",
							Value: `/var/hostlog/apport.log`,
						},
						{
							Name:  "job",
							Value: "varlogs",
						},
					},
				},
				{
					Floats: []promql.FPoint{
						{
							T: 1568404331324,
							F: 3.45,
						},
						{
							T: 1568404331339,
							F: 4.45,
						},
					},
					Metric: []labels.Label{
						{
							Name:  "filename",
							Value: `/var/hostlog/syslog`,
						},
						{
							Name:  "job",
							Value: "varlogs",
						},
					},
				},
			},
			`{
				"data": {
					` + statsResultString + `
					"resultType": "matrix",
					"result": [
						{
						"metric": {
							"filename": "\/var\/hostlog\/apport.log",
							"job": "varlogs"
						},
						"values": [
							[
								1568404331.324,
								"0.013333333333333334"
							]
							]
						},
						{
						"metric": {
							"filename": "\/var\/hostlog\/syslog",
							"job": "varlogs"
						},
						"values": [
								[
									1568404331.324,
									"3.45"
								],
								[
									1568404331.339,
									"4.45"
								]
							]
						}
					]
				},
				"status": "success"
			}`,
		},
	}
	codec := RequestProtobufCodec{}
	for i, queryTest := range queryTests {
		u := &url.URL{Path: "/loki/api/v1/query_range"}
		httpReq := &http.Request{
			Method:     "GET",
			RequestURI: u.String(),
			URL:        u,
		}
		req, err := codec.DecodeRequest(context.TODO(), httpReq, nil)
		require.NoError(t, err)

		// parser.Value -> queryrange.QueryResponse
		var b bytes.Buffer
		result := logqlmodel.Result{
			Data:       queryTest.actual,
			Statistics: statsResult,
		}
		err = WriteQueryResponseProtobuf(&logql.LiteralParams{}, result, &b)
		require.NoError(t, err)

		// queryrange.QueryResponse -> queryrangebase.Response
		querierResp := &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(&b),
			Header: http.Header{
				"Content-Type": []string{ProtobufType},
			},
		}
		resp, err := codec.DecodeResponse(context.TODO(), querierResp, req)
		require.NoError(t, err)

		// queryrange.Response -> JSON
		httpResp, err := codec.EncodeResponse(context.TODO(), httpReq, resp)
		require.NoError(t, err)

		body, _ := io.ReadAll(httpResp.Body)
		require.JSONEqf(t, queryTest.expected, string(body), "Protobuf Decode Query Test %d failed", i)
	}

}

func Test_codec_EncodeRequest(t *testing.T) {
	// we only accept LokiRequest.
	got, err := DefaultCodec.EncodeRequest(context.TODO(), &queryrangebase.PrometheusRequest{})
	require.Error(t, err)
	require.Nil(t, got)

	ctx := context.Background()
	toEncode := &LokiRequest{
		Query:     `{foo="bar"}`,
		Limit:     200,
		Step:      86400000, // nanoseconds
		Interval:  10000000, // nanoseconds
		Direction: logproto.FORWARD,
		Path:      "/query_range",
		StartTs:   start,
		EndTs:     end,
	}
	got, err = DefaultCodec.EncodeRequest(ctx, toEncode)
	require.NoError(t, err)
	require.Equal(t, ctx, got.Context())
	require.Equal(t, "/loki/api/v1/query_range", got.URL.Path)
	require.Equal(t, fmt.Sprintf("%d", start.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", end.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{foo="bar"}`, got.URL.Query().Get("query"))
	require.Equal(t, fmt.Sprintf("%d", 200), got.URL.Query().Get("limit"))
	require.Equal(t, `FORWARD`, got.URL.Query().Get("direction"))
	require.Equal(t, "86400.000000", got.URL.Query().Get("step"))
	require.Equal(t, "10000.000000", got.URL.Query().Get("interval"))

	// testing a full roundtrip
	req, err := DefaultCodec.DecodeRequest(context.TODO(), got, nil)
	require.NoError(t, err)
	require.Equal(t, toEncode.Query, req.(*LokiRequest).Query)
	require.Equal(t, toEncode.Step, req.(*LokiRequest).Step)
	require.Equal(t, toEncode.Interval, req.(*LokiRequest).Interval)
	require.Equal(t, toEncode.StartTs, req.(*LokiRequest).StartTs)
	require.Equal(t, toEncode.EndTs, req.(*LokiRequest).EndTs)
	require.Equal(t, toEncode.Direction, req.(*LokiRequest).Direction)
	require.Equal(t, toEncode.Limit, req.(*LokiRequest).Limit)
	require.Equal(t, "/loki/api/v1/query_range", req.(*LokiRequest).Path)
}

func Test_codec_series_EncodeRequest(t *testing.T) {
	got, err := DefaultCodec.EncodeRequest(context.TODO(), &queryrangebase.PrometheusRequest{})
	require.Error(t, err)
	require.Nil(t, got)

	ctx := context.Background()
	toEncode := &LokiSeriesRequest{
		Match:   []string{`{foo="bar"}`},
		Path:    "/series",
		StartTs: start,
		EndTs:   end,
	}
	got, err = DefaultCodec.EncodeRequest(ctx, toEncode)
	require.NoError(t, err)
	require.Equal(t, ctx, got.Context())
	require.Equal(t, "/loki/api/v1/series", got.URL.Path)
	require.Equal(t, fmt.Sprintf("%d", start.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", end.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{foo="bar"}`, got.URL.Query().Get("match[]"))

	// testing a full roundtrip
	req, err := DefaultCodec.DecodeRequest(context.TODO(), got, nil)
	require.NoError(t, err)
	require.Equal(t, toEncode.Match, req.(*LokiSeriesRequest).Match)
	require.Equal(t, toEncode.StartTs, req.(*LokiSeriesRequest).StartTs)
	require.Equal(t, toEncode.EndTs, req.(*LokiSeriesRequest).EndTs)
	require.Equal(t, "/loki/api/v1/series", req.(*LokiSeriesRequest).Path)
}

func Test_codec_labels_EncodeRequest(t *testing.T) {
	ctx := context.Background()
	toEncode := &LokiLabelNamesRequest{
		Path:    "/loki/api/v1/labels",
		StartTs: start,
		EndTs:   end,
	}
	got, err := DefaultCodec.EncodeRequest(ctx, toEncode)
	require.NoError(t, err)
	require.Equal(t, ctx, got.Context())
	require.Equal(t, "/loki/api/v1/labels", got.URL.Path)
	require.Equal(t, fmt.Sprintf("%d", start.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", end.UnixNano()), got.URL.Query().Get("end"))

	// testing a full roundtrip
	req, err := DefaultCodec.DecodeRequest(context.TODO(), got, nil)
	require.NoError(t, err)
	require.Equal(t, toEncode.StartTs, req.(*LokiLabelNamesRequest).StartTs)
	require.Equal(t, toEncode.EndTs, req.(*LokiLabelNamesRequest).EndTs)
	require.Equal(t, "/loki/api/v1/labels", req.(*LokiLabelNamesRequest).Path)

	// Test labels values endpoint
	toEncode = &LokiLabelNamesRequest{
		Path:    "/loki/api/v1/labels/__name__/values",
		StartTs: start,
		EndTs:   end,
		Query:   `{foo="bar"}`,
	}
	got, err = DefaultCodec.EncodeRequest(ctx, toEncode)
	require.NoError(t, err)
	require.Equal(t, ctx, got.Context())
	require.Equal(t, "/loki/api/v1/labels/__name__/values", got.URL.Path)
	require.Equal(t, fmt.Sprintf("%d", start.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", end.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{foo="bar"}`, got.URL.Query().Get("query"))

	// testing a full roundtrip
	req, err = DefaultCodec.DecodeRequest(context.TODO(), got, nil)
	require.NoError(t, err)
	require.Equal(t, toEncode.StartTs, req.(*LokiLabelNamesRequest).StartTs)
	require.Equal(t, toEncode.EndTs, req.(*LokiLabelNamesRequest).EndTs)
	require.Equal(t, toEncode.Query, req.(*LokiLabelNamesRequest).Query)
	require.Equal(t, "/loki/api/v1/labels/__name__/values", req.(*LokiLabelNamesRequest).Path)
}

func Test_codec_labels_DecodeRequest(t *testing.T) {
	ctx := context.Background()
	u, err := url.Parse(`/loki/api/v1/labels/__name__/values?start=1575285010000000010&end=1575288610000000010&query={foo="bar"}`)
	require.NoError(t, err)

	r := &http.Request{URL: u}
	req, err := DefaultCodec.DecodeRequest(context.TODO(), r, nil)
	require.NoError(t, err)
	require.Equal(t, start, req.(*LokiLabelNamesRequest).StartTs)
	require.Equal(t, end, req.(*LokiLabelNamesRequest).EndTs)
	require.Equal(t, `{foo="bar"}`, req.(*LokiLabelNamesRequest).Query)
	require.Equal(t, "/loki/api/v1/labels/__name__/values", req.(*LokiLabelNamesRequest).Path)

	got, err := DefaultCodec.EncodeRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, ctx, got.Context())
	require.Equal(t, "/loki/api/v1/labels/__name__/values", got.URL.Path)
	require.Equal(t, fmt.Sprintf("%d", start.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", end.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{foo="bar"}`, got.URL.Query().Get("query"))
}

func Test_codec_index_stats_EncodeRequest(t *testing.T) {
	from, through := util.RoundToMilliseconds(start, end)
	toEncode := &logproto.IndexStatsRequest{
		From:     from,
		Through:  through,
		Matchers: `{job="foo"}`,
	}
	got, err := DefaultCodec.EncodeRequest(context.Background(), toEncode)
	require.Nil(t, err)
	require.Equal(t, fmt.Sprintf("%d", from.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", through.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{job="foo"}`, got.URL.Query().Get("query"))
}

func Test_codec_seriesVolume_EncodeRequest(t *testing.T) {
	from, through := util.RoundToMilliseconds(start, end)
	toEncode := &logproto.VolumeRequest{
		From:         from,
		Through:      through,
		Matchers:     `{job="foo"}`,
		Limit:        20,
		Step:         30 * 1e6,
		TargetLabels: []string{"foo", "bar"},
	}
	got, err := DefaultCodec.EncodeRequest(context.Background(), toEncode)
	require.Nil(t, err)
	require.Equal(t, fmt.Sprintf("%d", from.UnixNano()), got.URL.Query().Get("start"))
	require.Equal(t, fmt.Sprintf("%d", through.UnixNano()), got.URL.Query().Get("end"))
	require.Equal(t, `{job="foo"}`, got.URL.Query().Get("query"))
	require.Equal(t, "20", got.URL.Query().Get("limit"))
	require.Equal(t, fmt.Sprintf("%f", float64(toEncode.Step/1e3)), got.URL.Query().Get("step"))
	require.Equal(t, `foo,bar`, got.URL.Query().Get("targetLabels"))
}

func Test_codec_EncodeResponse(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		res     queryrangebase.Response
		body    string
		wantErr bool
	}{
		{"error", "/loki/api/v1/query_range", &badResponse{}, "", true},
		{
			"prom", "/loki/api/v1/query_range",
			&LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: loghttp.QueryStatusSuccess,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeMatrix,
						Result:     sampleStreams,
					},
				},
				Statistics: statsResult,
			}, matrixString, false},
		{
			"loki v1", "/loki/api/v1/query_range",
			&LokiResponse{
				Status:    loghttp.QueryStatusSuccess,
				Direction: logproto.FORWARD,
				Limit:     100,
				Version:   uint32(loghttp.VersionV1),
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result:     logStreams,
				},
				Statistics: statsResult,
			}, streamsString, false,
		},
		{
			"loki legacy", "/api/promt/query",
			&LokiResponse{
				Status:    loghttp.QueryStatusSuccess,
				Direction: logproto.FORWARD,
				Limit:     100,
				Version:   uint32(loghttp.VersionLegacy),
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result:     logStreams,
				},
				Statistics: statsResult,
			}, streamsStringLegacy, false,
		},
		{
			"loki series", "/loki/api/v1/series",
			&LokiSeriesResponse{
				Status:  "success",
				Version: uint32(loghttp.VersionV1),
				Data:    seriesData,
			}, seriesString, false,
		},
		{
			"loki labels", "/loki/api/v1/labels",
			&LokiLabelNamesResponse{
				Status:  "success",
				Version: uint32(loghttp.VersionV1),
				Data:    labelsData,
			}, labelsString, false,
		},
		{
			"loki labels legacy", "/api/prom/label",
			&LokiLabelNamesResponse{
				Status:  "success",
				Version: uint32(loghttp.VersionLegacy),
				Data:    labelsData,
			}, labelsLegacyString, false,
		},
		{
			"index stats", "/loki/api/v1/index/stats",
			&IndexStatsResponse{
				Response: &logproto.IndexStatsResponse{
					Streams: 1,
					Chunks:  2,
					Bytes:   3,
					Entries: 4,
				},
			}, indexStatsString, false,
		},
		{
			"volume", "/loki/api/v1/index/volume",
			&VolumeResponse{
				Response: &logproto.VolumeResponse{
					Volumes: []logproto.Volume{
						{Name: `{foo="bar"}`, Volume: 38},
					},
					Limit: 100,
				},
			}, seriesVolumeString, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &url.URL{Path: tt.path}
			req := &http.Request{
				Method:     "GET",
				RequestURI: u.String(),
				URL:        u,
			}
			got, err := DefaultCodec.EncodeResponse(context.TODO(), req, tt.res)
			if (err != nil) != tt.wantErr {
				t.Errorf("codec.EncodeResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err == nil {
				require.Equal(t, 200, got.StatusCode)
				body, err := io.ReadAll(got.Body)
				require.Nil(t, err)
				bodyString := string(body)
				require.JSONEq(t, tt.body, bodyString)
			}
		})
	}
}

func Test_codec_MergeResponse(t *testing.T) {
	tests := []struct {
		name         string
		responses    []queryrangebase.Response
		want         queryrangebase.Response
		errorMessage string
	}{
		{
			"empty",
			[]queryrangebase.Response{},
			nil,
			"merging responses requires at least one response",
		},
		{
			"unknown response",
			[]queryrangebase.Response{&badResponse{}},
			nil,
			"unknown response type (*queryrange.badResponse) in merging responses",
		},
		{
			"prom",
			[]queryrangebase.Response{
				&LokiPromResponse{
					Response: &queryrangebase.PrometheusResponse{
						Status: loghttp.QueryStatusSuccess,
						Data: queryrangebase.PrometheusData{
							ResultType: loghttp.ResultTypeMatrix,
							Result:     sampleStreams,
						},
					},
				},
			},
			&LokiPromResponse{
				Statistics: stats.Result{Summary: stats.Summary{Splits: 1}},
				Response: &queryrangebase.PrometheusResponse{
					Status: loghttp.QueryStatusSuccess,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeMatrix,
						Result:     sampleStreams,
					},
				},
			},
			"",
		},
		{
			"loki backward",
			[]queryrangebase.Response{
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.BACKWARD,
					Limit:     100,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 2), Line: "2"},
									{Timestamp: time.Unix(0, 1), Line: "1"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 6), Line: "6"},
									{Timestamp: time.Unix(0, 5), Line: "5"},
								},
							},
						},
					},
				},
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.BACKWARD,
					Limit:     100,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 10), Line: "10"},
									{Timestamp: time.Unix(0, 9), Line: "9"},
									{Timestamp: time.Unix(0, 9), Line: "9"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 16), Line: "16"},
									{Timestamp: time.Unix(0, 15), Line: "15"},
								},
							},
						},
					},
				},
			},
			&LokiResponse{
				Status:     loghttp.QueryStatusSuccess,
				Direction:  logproto.BACKWARD,
				Limit:      100,
				Version:    1,
				Statistics: stats.Result{Summary: stats.Summary{Splits: 2}},
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result: []logproto.Stream{
						{
							Labels: `{foo="bar", level="error"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 10), Line: "10"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
								{Timestamp: time.Unix(0, 2), Line: "2"},
								{Timestamp: time.Unix(0, 1), Line: "1"},
							},
						},
						{
							Labels: `{foo="bar", level="debug"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 16), Line: "16"},
								{Timestamp: time.Unix(0, 15), Line: "15"},
								{Timestamp: time.Unix(0, 6), Line: "6"},
								{Timestamp: time.Unix(0, 5), Line: "5"},
							},
						},
					},
				},
			},
			"",
		},
		{
			"loki backward limited",
			[]queryrangebase.Response{
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.BACKWARD,
					Limit:     6,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 10), Line: "10"},
									{Timestamp: time.Unix(0, 9), Line: "9"},
									{Timestamp: time.Unix(0, 9), Line: "9"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 16), Line: "16"},
									{Timestamp: time.Unix(0, 15), Line: "15"},
								},
							},
						},
					},
				},
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.BACKWARD,
					Limit:     6,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 2), Line: "2"},
									{Timestamp: time.Unix(0, 1), Line: "1"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 6), Line: "6"},
									{Timestamp: time.Unix(0, 5), Line: "5"},
								},
							},
						},
					},
				},
			},
			&LokiResponse{
				Status:     loghttp.QueryStatusSuccess,
				Direction:  logproto.BACKWARD,
				Limit:      6,
				Version:    1,
				Statistics: stats.Result{Summary: stats.Summary{Splits: 2}},
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result: []logproto.Stream{
						{
							Labels: `{foo="bar", level="error"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 10), Line: "10"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
							},
						},
						{
							Labels: `{foo="bar", level="debug"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 16), Line: "16"},
								{Timestamp: time.Unix(0, 15), Line: "15"},
								{Timestamp: time.Unix(0, 6), Line: "6"},
							},
						},
					},
				},
			},
			"",
		},
		{
			"loki forward",
			[]queryrangebase.Response{
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.FORWARD,
					Limit:     100,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 1), Line: "1"},
									{Timestamp: time.Unix(0, 2), Line: "2"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 5), Line: "5"},
									{Timestamp: time.Unix(0, 6), Line: "6"},
								},
							},
						},
					},
				},
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.FORWARD,
					Limit:     100,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 9), Line: "9"},
									{Timestamp: time.Unix(0, 10), Line: "10"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 15), Line: "15"},
									{Timestamp: time.Unix(0, 15), Line: "15"},
									{Timestamp: time.Unix(0, 16), Line: "16"},
								},
							},
						},
					},
				},
			},
			&LokiResponse{
				Status:     loghttp.QueryStatusSuccess,
				Direction:  logproto.FORWARD,
				Limit:      100,
				Version:    1,
				Statistics: stats.Result{Summary: stats.Summary{Splits: 2}},
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result: []logproto.Stream{
						{
							Labels: `{foo="bar", level="debug"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 5), Line: "5"},
								{Timestamp: time.Unix(0, 6), Line: "6"},
								{Timestamp: time.Unix(0, 15), Line: "15"},
								{Timestamp: time.Unix(0, 15), Line: "15"},
								{Timestamp: time.Unix(0, 16), Line: "16"},
							},
						},
						{
							Labels: `{foo="bar", level="error"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 1), Line: "1"},
								{Timestamp: time.Unix(0, 2), Line: "2"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
								{Timestamp: time.Unix(0, 10), Line: "10"},
							},
						},
					},
				},
			},
			"",
		},
		{
			"loki forward limited",
			[]queryrangebase.Response{
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.FORWARD,
					Limit:     5,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 1), Line: "1"},
									{Timestamp: time.Unix(0, 2), Line: "2"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 5), Line: "5"},
									{Timestamp: time.Unix(0, 6), Line: "6"},
								},
							},
						},
					},
				},
				&LokiResponse{
					Status:    loghttp.QueryStatusSuccess,
					Direction: logproto.FORWARD,
					Limit:     5,
					Version:   1,
					Data: LokiData{
						ResultType: loghttp.ResultTypeStream,
						Result: []logproto.Stream{
							{
								Labels: `{foo="bar", level="error"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 9), Line: "9"},
									{Timestamp: time.Unix(0, 10), Line: "10"},
								},
							},
							{
								Labels: `{foo="bar", level="debug"}`,
								Entries: []logproto.Entry{
									{Timestamp: time.Unix(0, 15), Line: "15"},
									{Timestamp: time.Unix(0, 15), Line: "15"},
									{Timestamp: time.Unix(0, 16), Line: "16"},
								},
							},
						},
					},
				},
			},
			&LokiResponse{
				Status:     loghttp.QueryStatusSuccess,
				Direction:  logproto.FORWARD,
				Limit:      5,
				Version:    1,
				Statistics: stats.Result{Summary: stats.Summary{Splits: 2}},
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result: []logproto.Stream{
						{
							Labels: `{foo="bar", level="debug"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 5), Line: "5"},
								{Timestamp: time.Unix(0, 6), Line: "6"},
							},
						},
						{
							Labels: `{foo="bar", level="error"}`,
							Entries: []logproto.Entry{
								{Timestamp: time.Unix(0, 1), Line: "1"},
								{Timestamp: time.Unix(0, 2), Line: "2"},
								{Timestamp: time.Unix(0, 9), Line: "9"},
							},
						},
					},
				},
			},
			"",
		},
		{
			"loki series",
			[]queryrangebase.Response{
				&LokiSeriesResponse{
					Status:  "success",
					Version: 1,
					Data: []logproto.SeriesIdentifier{
						{
							Labels: map[string]string{"filename": "/var/hostlog/apport.log", "job": "varlogs"},
						},
						{
							Labels: map[string]string{"filename": "/var/hostlog/test.log", "job": "varlogs"},
						},
					},
				},
				&LokiSeriesResponse{
					Status:  "success",
					Version: 1,
					Data: []logproto.SeriesIdentifier{
						{
							Labels: map[string]string{"filename": "/var/hostlog/apport.log", "job": "varlogs"},
						},
						{
							Labels: map[string]string{"filename": "/var/hostlog/other.log", "job": "varlogs"},
						},
					},
				},
			},
			&LokiSeriesResponse{
				Statistics: stats.Result{Summary: stats.Summary{Splits: 2}},
				Status:     "success",
				Version:    1,
				Data: []logproto.SeriesIdentifier{
					{
						Labels: map[string]string{"filename": "/var/hostlog/apport.log", "job": "varlogs"},
					},
					{
						Labels: map[string]string{"filename": "/var/hostlog/test.log", "job": "varlogs"},
					},
					{
						Labels: map[string]string{"filename": "/var/hostlog/other.log", "job": "varlogs"},
					},
				},
			},
			"",
		},
		{
			"loki labels",
			[]queryrangebase.Response{
				&LokiLabelNamesResponse{
					Status:  "success",
					Version: 1,
					Data:    []string{"foo", "bar", "buzz"},
				},
				&LokiLabelNamesResponse{
					Status:  "success",
					Version: 1,
					Data:    []string{"foo", "bar", "buzz"},
				},
				&LokiLabelNamesResponse{
					Status:  "success",
					Version: 1,
					Data:    []string{"foo", "blip", "blop"},
				},
			},
			&LokiLabelNamesResponse{
				Statistics: stats.Result{Summary: stats.Summary{Splits: 3}},
				Status:     "success",
				Version:    1,
				Data:       []string{"foo", "bar", "buzz", "blip", "blop"},
			},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DefaultCodec.MergeResponse(tt.responses...)
			if tt.errorMessage != "" {
				require.ErrorContains(t, err, tt.errorMessage)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

type badResponse struct{}

func (badResponse) Reset()                                                 {}
func (badResponse) String() string                                         { return "noop" }
func (badResponse) ProtoMessage()                                          {}
func (badResponse) GetHeaders() []*queryrangebase.PrometheusResponseHeader { return nil }

type badReader struct{}

func (badReader) Read(_ []byte) (n int, err error) {
	return 0, errors.New("")
}

var (
	statsResultString = `"stats" : {
		"ingester" : {
			"store": {
				"chunk":{
					"compressedBytes": 1,
					"decompressedBytes": 2,
					"decompressedLines": 3,
					"decompressedNonIndexedLabelsBytes": 0,
					"headChunkBytes": 4,
					"headChunkLines": 5,
					"headChunkNonIndexedLabelsBytes": 0,
					"postFilterLines": 0,
					"totalDuplicates": 8
				},
				"chunksDownloadTime": 0,
				"totalChunksRef": 0,
				"totalChunksDownloaded": 0
			},
			"totalBatches": 6,
			"totalChunksMatched": 7,
			"totalLinesSent": 9,
			"totalReached": 10
		},
		"querier": {
			"store" : {
				"chunk": {
					"compressedBytes": 11,
					"decompressedBytes": 12,
					"decompressedLines": 13,
					"decompressedNonIndexedLabelsBytes": 0,
					"headChunkBytes": 14,
					"headChunkLines": 15,
					"headChunkNonIndexedLabelsBytes": 0,
                    "postFilterLines": 0,
					"totalDuplicates": 19
				},
				"chunksDownloadTime": 16,
				"totalChunksRef": 17,
				"totalChunksDownloaded": 18
			}
		},
		"cache": {
			"chunk": {
				"entriesFound": 0,
				"entriesRequested": 0,
				"entriesStored": 0,
				"bytesReceived": 0,
				"bytesSent": 0,
				"requests": 0,
				"downloadTime": 0
			},
			"index": {
				"entriesFound": 0,
				"entriesRequested": 0,
				"entriesStored": 0,
				"bytesReceived": 0,
				"bytesSent": 0,
				"requests": 0,
				"downloadTime": 0
			},
		    "statsResult": {
				"entriesFound": 0,
				"entriesRequested": 0,
				"entriesStored": 0,
				"bytesReceived": 0,
				"bytesSent": 0,
				"requests": 0,
				"downloadTime": 0
			},
			"result": {
				"entriesFound": 0,
				"entriesRequested": 0,
				"entriesStored": 0,
				"bytesReceived": 0,
				"bytesSent": 0,
				"requests": 0,
				"downloadTime": 0
			}
		},
		"summary": {
			"bytesProcessedPerSecond": 20,
			"execTime": 22,
			"linesProcessedPerSecond": 23,
			"queueTime": 21,
			"shards": 0,
			"splits": 0,
			"subqueries": 0,
			"totalBytesProcessed": 24,
			"totalEntriesReturned": 10,
			"totalLinesProcessed": 25,
			"totalNonIndexedLabelsBytesProcessed": 0,
            "totalPostFilterLines": 0
		}
	},`
	matrixString = `{
	"data": {
	  ` + statsResultString + `
	  "resultType": "matrix",
	  "result": [
		{
		  "metric": {
			"filename": "\/var\/hostlog\/apport.log",
			"job": "varlogs"
		  },
		  "values": [
			  [
				1568404331.324,
				"0.013333333333333334"
			  ]
			]
		},
		{
		  "metric": {
			"filename": "\/var\/hostlog\/syslog",
			"job": "varlogs"
		  },
		  "values": [
				[
					1568404331.324,
					"3.45"
				],
				[
					1568404331.339,
					"4.45"
				]
			]
		}
	  ]
	},
	"status": "success"
  }`
	matrixStringEmptyResult = `{
	"data": {
	  ` + statsResultString + `
	  "resultType": "matrix",
	  "result": []
	},
	"status": "success"
  }`
	vectorStringEmptyResult = `{
	"data": {
	  ` + statsResultString + `
	  "resultType": "vector",
	  "result": []
	},
	"status": "success"
  }`

	sampleStreams = []queryrangebase.SampleStream{
		{
			Labels:  []logproto.LabelAdapter{{Name: "filename", Value: "/var/hostlog/apport.log"}, {Name: "job", Value: "varlogs"}},
			Samples: []logproto.LegacySample{{Value: 0.013333333333333334, TimestampMs: 1568404331324}},
		},
		{
			Labels:  []logproto.LabelAdapter{{Name: "filename", Value: "/var/hostlog/syslog"}, {Name: "job", Value: "varlogs"}},
			Samples: []logproto.LegacySample{{Value: 3.45, TimestampMs: 1568404331324}, {Value: 4.45, TimestampMs: 1568404331339}},
		},
	}
	streamsString = `{
		"status": "success",
		"data": {
			` + statsResultString + `
			"resultType": "streams",
			"result": [
				{
					"stream": {
						"test": "test"
					},
					"values":[
						[ "123456789012345", "super line"]
					]
				},
				{
					"stream": {
						"test": "test2"
					},
					"values":[
						[ "123456789012346", "super line2"]
					]
				}
			]
		}
	}`
	streamsStringLegacy = `{
		` + statsResultString + `"streams":[{"labels":"{test=\"test\"}","entries":[{"ts":"1970-01-02T10:17:36.789012345Z","line":"super line"}]},{"labels":"{test=\"test2\"}","entries":[{"ts":"1970-01-02T10:17:36.789012346Z","line":"super line2"}]}]}`
	logStreams = []logproto.Stream{
		{
			Labels: `{test="test"}`,
			Entries: []logproto.Entry{
				{
					Line:      "super line",
					Timestamp: time.Unix(0, 123456789012345).UTC(),
				},
			},
		},
		{
			Labels: `{test="test2"}`,
			Entries: []logproto.Entry{
				{
					Line:      "super line2",
					Timestamp: time.Unix(0, 123456789012346).UTC(),
				},
			},
		},
	}
	seriesString = `{
		"status": "success",
		"data": [
			{"filename": "/var/hostlog/apport.log", "job": "varlogs"},
			{"filename": "/var/hostlog/test.log", "job": "varlogs"}
		]
	}`
	seriesData = []logproto.SeriesIdentifier{
		{
			Labels: map[string]string{"filename": "/var/hostlog/apport.log", "job": "varlogs"},
		},
		{
			Labels: map[string]string{"filename": "/var/hostlog/test.log", "job": "varlogs"},
		},
	}
	labelsString = `{
		"status": "success",
		"data": [
			"foo",
			"bar"
		]
	}`
	labelsLegacyString = `{
		"values": [
			"foo",
			"bar"
		]
	}`
	indexStatsString = `{
		"streams": 1,
		"chunks": 2,
		"bytes": 3,
		"entries": 4
		}`
	seriesVolumeString = `{
    "limit": 100,
    "volumes": [
      {
        "name": "{foo=\"bar\"}",
        "volume": 38
      }
    ]
  }`
	labelsData  = []string{"foo", "bar"}
	statsResult = stats.Result{
		Summary: stats.Summary{
			BytesProcessedPerSecond: 20,
			QueueTime:               21,
			ExecTime:                22,
			LinesProcessedPerSecond: 23,
			TotalBytesProcessed:     24,
			TotalLinesProcessed:     25,
			TotalEntriesReturned:    10,
			TotalPostFilterLines:    0,
		},
		Querier: stats.Querier{
			Store: stats.Store{
				Chunk: stats.Chunk{
					CompressedBytes:   11,
					DecompressedBytes: 12,
					DecompressedLines: 13,
					HeadChunkBytes:    14,
					HeadChunkLines:    15,
					PostFilterLines:   0,
					TotalDuplicates:   19,
				},
				ChunksDownloadTime:    16,
				TotalChunksRef:        17,
				TotalChunksDownloaded: 18,
			},
		},

		Ingester: stats.Ingester{
			Store: stats.Store{
				Chunk: stats.Chunk{
					CompressedBytes:   1,
					DecompressedBytes: 2,
					DecompressedLines: 3,
					HeadChunkBytes:    4,
					HeadChunkLines:    5,
					PostFilterLines:   0,
					TotalDuplicates:   8,
				},
			},
			TotalBatches:       6,
			TotalChunksMatched: 7,
			TotalLinesSent:     9,
			TotalReached:       10,
		},

		Caches: stats.Caches{
			Chunk:  stats.Cache{},
			Index:  stats.Cache{},
			Result: stats.Cache{},
		},
	}
)

func BenchmarkResponseMerge(b *testing.B) {
	const (
		resps         = 10
		streams       = 100
		logsPerStream = 1000
	)

	for _, tc := range []struct {
		desc  string
		limit uint32
		fn    func([]*LokiResponse, uint32, logproto.Direction) []logproto.Stream
	}{
		{
			"mergeStreams unlimited",
			uint32(streams * logsPerStream),
			mergeStreams,
		},
		{
			"mergeOrderedNonOverlappingStreams unlimited",
			uint32(streams * logsPerStream),
			mergeOrderedNonOverlappingStreams,
		},
		{
			"mergeStreams limited",
			uint32(streams*logsPerStream - 1),
			mergeStreams,
		},
		{
			"mergeOrderedNonOverlappingStreams limited",
			uint32(streams*logsPerStream - 1),
			mergeOrderedNonOverlappingStreams,
		},
	} {
		input := mkResps(resps, streams, logsPerStream, logproto.FORWARD)
		b.Run(tc.desc, func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				tc.fn(input, tc.limit, logproto.FORWARD)
			}
		})
	}
}

func mkResps(nResps, nStreams, nLogs int, direction logproto.Direction) (resps []*LokiResponse) {
	for i := 0; i < nResps; i++ {
		r := &LokiResponse{}
		for j := 0; j < nStreams; j++ {
			stream := logproto.Stream{
				Labels: fmt.Sprintf(`{foo="%d"}`, j),
			}
			// split nLogs evenly across all responses
			for k := i * (nLogs / nResps); k < (i+1)*(nLogs/nResps); k++ {
				stream.Entries = append(stream.Entries, logproto.Entry{
					Timestamp: time.Unix(int64(k), 0),
					Line:      fmt.Sprintf("%d", k),
				})

				if direction == logproto.BACKWARD {
					for x, y := 0, len(stream.Entries)-1; x < len(stream.Entries)/2; x, y = x+1, y-1 {
						stream.Entries[x], stream.Entries[y] = stream.Entries[y], stream.Entries[x]
					}
				}
			}
			r.Data.Result = append(r.Data.Result, stream)
		}
		resps = append(resps, r)
	}
	return resps
}

type buffer struct {
	buff []byte
	io.ReadCloser
}

func (b *buffer) Bytes() []byte {
	return b.buff
}

func Benchmark_CodecDecodeLogs(b *testing.B) {
	ctx := context.Background()
	u := &url.URL{Path: "/loki/api/v1/query_range"}
	req := &http.Request{
		Method:     "GET",
		RequestURI: u.String(), // This is what the httpgrpc code looks at.
		URL:        u,
	}
	resp, err := DefaultCodec.EncodeResponse(ctx, req, &LokiResponse{
		Status:    loghttp.QueryStatusSuccess,
		Direction: logproto.BACKWARD,
		Version:   uint32(loghttp.VersionV1),
		Limit:     1000,
		Data: LokiData{
			ResultType: loghttp.ResultTypeStream,
			Result:     generateStream(),
		},
	})
	require.Nil(b, err)

	buf, err := io.ReadAll(resp.Body)
	require.Nil(b, err)
	reader := bytes.NewReader(buf)
	resp.Body = &buffer{
		ReadCloser: io.NopCloser(reader),
		buff:       buf,
	}
	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		_, _ = reader.Seek(0, io.SeekStart)
		result, err := DefaultCodec.DecodeResponse(ctx, resp, &LokiRequest{
			Limit:     100,
			StartTs:   start,
			EndTs:     end,
			Direction: logproto.BACKWARD,
			Path:      u.String(),
		})
		require.Nil(b, err)
		require.NotNil(b, result)
	}
}

func Benchmark_CodecDecodeSamples(b *testing.B) {
	ctx := context.Background()
	u := &url.URL{Path: "/loki/api/v1/query_range"}
	req := &http.Request{
		Method:     "GET",
		RequestURI: u.String(), // This is what the httpgrpc code looks at.
		URL:        u,
	}
	resp, err := DefaultCodec.EncodeResponse(ctx, req, &LokiPromResponse{
		Response: &queryrangebase.PrometheusResponse{
			Status: loghttp.QueryStatusSuccess,
			Data: queryrangebase.PrometheusData{
				ResultType: loghttp.ResultTypeMatrix,
				Result:     generateMatrix(),
			},
		},
	})
	require.Nil(b, err)

	buf, err := io.ReadAll(resp.Body)
	require.Nil(b, err)
	reader := bytes.NewReader(buf)
	resp.Body = io.NopCloser(reader)
	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		_, _ = reader.Seek(0, io.SeekStart)
		result, err := DefaultCodec.DecodeResponse(ctx, resp, &LokiRequest{
			Limit:     100,
			StartTs:   start,
			EndTs:     end,
			Direction: logproto.BACKWARD,
			Path:      u.String(),
		})
		require.NoError(b, err)
		require.NotNil(b, result)
	}
}

func Benchmark_CodecDecodeSeries(b *testing.B) {
	ctx := context.Background()
	benchmarks := []struct {
		accept string
	}{
		{accept: ProtobufType},
		{accept: JSONType},
	}

	for _, bm := range benchmarks {
		u := &url.URL{Path: "/loki/api/v1/series"}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Header: http.Header{
				"Accept": []string{bm.accept},
			},
		}
		resp, err := DefaultCodec.EncodeResponse(ctx, req, &LokiSeriesResponse{
			Status:     "200",
			Version:    1,
			Statistics: stats.Result{},
			Data:       generateSeries(),
		})
		require.Nil(b, err)

		buf, err := io.ReadAll(resp.Body)
		require.Nil(b, err)
		reader := bytes.NewReader(buf)
		resp.Body = io.NopCloser(reader)
		b.Run(bm.accept, func(b *testing.B) {
			b.ResetTimer()
			b.ReportAllocs()
			for n := 0; n < b.N; n++ {
				_, _ = reader.Seek(0, io.SeekStart)
				result, err := DefaultCodec.DecodeResponse(ctx, resp, &LokiSeriesRequest{
					StartTs: start,
					EndTs:   end,
					Path:    u.String(),
				})
				require.NoError(b, err)
				require.NotNil(b, result)
			}
		})
	}

}

func Benchmark_MergeResponses(b *testing.B) {
	var responses []queryrangebase.Response = make([]queryrangebase.Response, 100)
	for i := range responses {
		responses[i] = &LokiSeriesResponse{
			Status:     "200",
			Version:    1,
			Statistics: stats.Result{},
			Data:       generateSeries(),
		}
	}

	b.ResetTimer()
	b.ReportAllocs()

	for n := 0; n < b.N; n++ {
		result, err := DefaultCodec.MergeResponse(responses...)
		require.Nil(b, err)
		require.NotNil(b, result)
	}
}

func generateMatrix() (res []queryrangebase.SampleStream) {
	for i := 0; i < 100; i++ {
		s := queryrangebase.SampleStream{
			Labels:  []logproto.LabelAdapter{},
			Samples: []logproto.LegacySample{},
		}
		for j := 0; j < 1000; j++ {
			s.Samples = append(s.Samples, logproto.LegacySample{
				Value:       float64(j),
				TimestampMs: int64(j),
			})
		}
		res = append(res, s)
	}
	return res
}

func generateStream() (res []logproto.Stream) {
	for i := 0; i < 1000; i++ {
		s := logproto.Stream{
			Labels: fmt.Sprintf(`{foo="%d", buzz="bar", cluster="us-central2", namespace="loki-dev", container="query-frontend"}`, i),
		}
		for j := 0; j < 10; j++ {
			s.Entries = append(s.Entries, logproto.Entry{Timestamp: time.Now(), Line: fmt.Sprintf("%d\nyolo", j)})
		}
		res = append(res, s)
	}
	return res
}

func generateSeries() (res []logproto.SeriesIdentifier) {
	for i := 0; i < 1000; i++ {
		labels := make(map[string]string)
		for l := 0; l < 100; l++ {
			labels[fmt.Sprintf("%d-%d", i, l)] = strconv.Itoa(l)
		}
		res = append(res, logproto.SeriesIdentifier{Labels: labels})
	}
	return res
}
