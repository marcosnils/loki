package querier

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/loki/pkg/loghttp"
	loghttp_legacy "github.com/grafana/loki/pkg/loghttp/legacy"
	"github.com/grafana/loki/pkg/logql/marshal"
	marshal_legacy "github.com/grafana/loki/pkg/logql/marshal/legacy"

	"github.com/cortexproject/cortex/pkg/util"
	"github.com/go-kit/kit/log/level"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/httpgrpc/server"
)

const (
	defaultQueryLimit    = 100
	defaultSince         = 1 * time.Hour
	wsPingPeriod         = 1 * time.Second
	maxDelayForInTailing = 5
)

// nolint
func intParam(values url.Values, name string, def int) (int, error) {
	value := values.Get(name)
	if value == "" {
		return def, nil
	}

	return strconv.Atoi(value)
}

func unixNanoTimeParam(values url.Values, name string, def time.Time) (time.Time, error) {
	value := values.Get(name)
	if value == "" {
		return def, nil
	}

	if strings.Contains(value, ".") {
		if t, err := strconv.ParseFloat(value, 64); err == nil {
			s, ns := math.Modf(t)
			ns = math.Round(ns*1000) / 1000
			return time.Unix(int64(s), int64(ns*float64(time.Second))), nil
		}
	}
	nanos, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return ts, nil
		}
		return time.Time{}, err
	}
	if len(value) <= 10 {
		return time.Unix(nanos, 0), nil
	}
	return time.Unix(0, nanos), nil
}

// nolint
func directionParam(values url.Values, name string, def logproto.Direction) (logproto.Direction, error) {
	value := values.Get(name)
	if value == "" {
		return def, nil
	}

	d, ok := logproto.Direction_value[strings.ToUpper(value)]
	if !ok {
		return logproto.FORWARD, fmt.Errorf("invalid direction '%s'", value)
	}
	return logproto.Direction(d), nil
}

// defaultQueryRangeStep returns the default step used in the query range API,
// which is dinamically calculated based on the time range
func defaultQueryRangeStep(start time.Time, end time.Time) int {
	return int(math.Max(math.Floor(end.Sub(start).Seconds()/250), 1))
}

func httpRequestToInstantQueryRequest(httpRequest *http.Request) (*instantQueryRequest, error) {
	params := httpRequest.URL.Query()
	queryRequest := instantQueryRequest{
		query: params.Get("query"),
	}

	limit, err := intParam(params, "limit", defaultQueryLimit)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}
	queryRequest.limit = uint32(limit)

	queryRequest.ts, err = unixNanoTimeParam(params, "time", time.Now())
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	queryRequest.direction, err = directionParam(params, "direction", logproto.BACKWARD)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	return &queryRequest, nil
}

func httpRequestToRangeQueryRequest(httpRequest *http.Request) (*rangeQueryRequest, error) {
	var err error

	params := httpRequest.URL.Query()
	queryRequest := rangeQueryRequest{
		query: params.Get("query"),
	}

	queryRequest.limit, queryRequest.start, queryRequest.end, err = httpRequestToLookback(httpRequest)
	if err != nil {
		return nil, err
	}

	step, err := intParam(params, "step", defaultQueryRangeStep(queryRequest.start, queryRequest.end))
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}
	queryRequest.step = time.Duration(step) * time.Second

	queryRequest.direction, err = directionParam(params, "direction", logproto.BACKWARD)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	return &queryRequest, nil
}

func httpRequestToTailRequest(httpRequest *http.Request) (*logproto.TailRequest, error) {
	params := httpRequest.URL.Query()
	tailRequest := logproto.TailRequest{
		Query: params.Get("query"),
	}
	var err error
	tailRequest.Limit, tailRequest.Start, _, err = httpRequestToLookback(httpRequest)
	if err != nil {
		return nil, err
	}

	// delay_for is used to allow server to let slow loggers catch up.
	// Entries would be accumulated in a heap until they become older than now()-<delay_for>
	delayFor, err := intParam(params, "delay_for", 0)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	tailRequest.DelayFor = uint32(delayFor)

	return &tailRequest, nil
}

func httpRequestToLookback(httpRequest *http.Request) (limit uint32, start, end time.Time, err error) {
	params := httpRequest.URL.Query()
	now := time.Now()

	lim, err := intParam(params, "limit", defaultQueryLimit)
	if err != nil {
		return 0, time.Now(), time.Now(), httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}
	limit = uint32(lim)

	start, err = unixNanoTimeParam(params, "start", now.Add(-defaultSince))
	if err != nil {
		return 0, time.Now(), time.Now(), httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	end, err = unixNanoTimeParam(params, "end", now)
	if err != nil {
		return 0, time.Now(), time.Now(), httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}
	return
}

// parseRegexQuery parses regex and query querystring from httpRequest and returns the combined LogQL query.
// This is used only to keep regexp query string support until it gets fully deprecated.
func parseRegexQuery(httpRequest *http.Request) (string, error) {
	params := httpRequest.URL.Query()
	query := params.Get("query")
	regexp := params.Get("regexp")
	if regexp != "" {
		expr, err := logql.ParseLogSelector(query)
		if err != nil {
			return "", err
		}
		query = logql.NewFilterExpr(expr, labels.MatchRegexp, regexp).String()
	}
	return query, nil
}

type QueryResponse struct {
	ResultType promql.ValueType `json:"resultType"`
	Result     promql.Value     `json:"result"`
}

type rangeQueryRequest struct {
	query      string
	start, end time.Time
	step       time.Duration
	limit      uint32
	direction  logproto.Direction
}

type instantQueryRequest struct {
	query     string
	ts        time.Time
	limit     uint32
	direction logproto.Direction
}

// RangeQueryHandler is a http.HandlerFunc for range queries.
func (q *Querier) RangeQueryHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.QueryTimeout))
	defer cancel()

	request, err := httpRequestToRangeQueryRequest(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	query := q.engine.NewRangeQuery(q, request.query, request.start, request.end, request.step, request.direction, request.limit)
	result, err := query.Exec(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := marshal.WriteQueryResponseJSON(result, w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// InstantQueryHandler is a http.HandlerFunc for instant queries.
func (q *Querier) InstantQueryHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.QueryTimeout))
	defer cancel()

	request, err := httpRequestToInstantQueryRequest(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	query := q.engine.NewInstantQuery(q, request.query, request.ts, request.direction, request.limit)
	result, err := query.Exec(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := marshal.WriteQueryResponseJSON(result, w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// LogQueryHandler is a http.HandlerFunc for log only queries.
func (q *Querier) LogQueryHandler(w http.ResponseWriter, r *http.Request) {
	// Enforce the query timeout while querying backends
	ctx, cancel := context.WithDeadline(r.Context(), time.Now().Add(q.cfg.QueryTimeout))
	defer cancel()

	request, err := httpRequestToRangeQueryRequest(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}
	request.query, err = parseRegexQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	query := q.engine.NewRangeQuery(q, request.query, request.start, request.end, request.step, request.direction, request.limit)
	result, err := query.Exec(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := marshal_legacy.WriteQueryResponseJSON(result, w); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// LabelHandler is a http.HandlerFunc for handling label queries.
func (q *Querier) LabelHandler(w http.ResponseWriter, r *http.Request) {
	name, ok := mux.Vars(r)["name"]
	params := r.URL.Query()
	now := time.Now()
	req := &logproto.LabelRequest{
		Values: ok,
		Name:   name,
	}

	end, err := unixNanoTimeParam(params, "end", now)
	if err != nil {
		http.Error(w, httpgrpc.Errorf(http.StatusBadRequest, err.Error()).Error(), http.StatusBadRequest)
		return
	}
	req.End = &end

	start, err := unixNanoTimeParam(params, "start", end.Add(-6*time.Hour))
	if err != nil {
		http.Error(w, httpgrpc.Errorf(http.StatusBadRequest, err.Error()).Error(), http.StatusBadRequest)
		return
	}
	req.Start = &start

	resp, err := q.Label(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if loghttp.GetVersion(r.RequestURI) == loghttp.VersionV1 {
		err = marshal.WriteLabelResponseJSON(*resp, w)
	} else {
		err = marshal_legacy.WriteLabelResponseJSON(*resp, w)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// TailHandler is a http.HandlerFunc for handling tail queries.
func (q *Querier) TailHandler(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	tailRequestPtr, err := httpRequestToTailRequest(r)
	if err != nil {
		server.WriteError(w, err)
		return
	}

	tailRequestPtr.Query, err = parseRegexQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if tailRequestPtr.DelayFor > maxDelayForInTailing {
		server.WriteError(w, fmt.Errorf("delay_for can't be greater than %d", maxDelayForInTailing))
		level.Error(util.Logger).Log("Error in upgrading websocket", fmt.Sprintf("%v", err))
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		level.Error(util.Logger).Log("Error in upgrading websocket", fmt.Sprintf("%v", err))
		return
	}

	defer func() {
		if err := conn.Close(); err != nil {
			level.Error(util.Logger).Log("Error closing websocket", fmt.Sprintf("%v", err))
		}
	}()

	// response from httpRequestToQueryRequest is a ptr, if we keep passing pointer down the call then it would stay on
	// heap until connection to websocket stays open
	tailRequest := *tailRequestPtr

	tailer, err := q.Tail(r.Context(), &tailRequest)
	if err != nil {
		if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
			level.Error(util.Logger).Log("Error connecting to ingesters for tailing", fmt.Sprintf("%v", err))
		}
		return
	}
	defer func() {
		if err := tailer.close(); err != nil {
			level.Error(util.Logger).Log("Error closing Tailer", fmt.Sprintf("%v", err))
		}
	}()

	ticker := time.NewTicker(wsPingPeriod)
	defer ticker.Stop()

	var response *loghttp_legacy.TailResponse
	responseChan := tailer.getResponseChan()
	closeErrChan := tailer.getCloseErrorChan()

	for {
		select {
		case response = <-responseChan:
			var err error
			if loghttp.GetVersion(r.RequestURI) == loghttp.VersionV1 {
				err = marshal.WriteTailResponseJSON(*response, conn)
			} else {
				err = marshal_legacy.WriteTailResponseJSON(*response, conn)
			}
			if err != nil {
				level.Error(util.Logger).Log("Error writing to websocket", fmt.Sprintf("%v", err))
				if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
					level.Error(util.Logger).Log("Error writing close message to websocket", fmt.Sprintf("%v", err))
				}
				return
			}

		case err := <-closeErrChan:
			level.Error(util.Logger).Log("Error from iterator", fmt.Sprintf("%v", err))
			if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
				level.Error(util.Logger).Log("Error writing close message to websocket", fmt.Sprintf("%v", err))
			}
			return
		case <-ticker.C:
			// This is to periodically check whether connection is active, useful to clean up dead connections when there are no entries to send
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				level.Error(util.Logger).Log("Error writing ping message to websocket", fmt.Sprintf("%v", err))
				if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, err.Error())); err != nil {
					level.Error(util.Logger).Log("Error writing close message to websocket", fmt.Sprintf("%v", err))
				}
				return
			}
		}
	}
}
