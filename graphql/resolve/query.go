// Portions Copyright 2019 Dgraph Labs, Inc. are available under the Apache License v2.0.
// Portions Copyright 2022 Outcaste LLC are available under the Sustainable License v1.0.

package resolve

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/golang/glog"
	otrace "go.opencensus.io/trace"

	"github.com/outcaste-io/outserv/edgraph"
	"github.com/outcaste-io/outserv/graphql/dgraph"
	"github.com/outcaste-io/outserv/graphql/schema"
	"github.com/outcaste-io/outserv/protos/pb"
	"github.com/outcaste-io/outserv/x"
)

var errNotScalar = errors.New("provided value is not a scalar, can't convert it to string")

// A QueryResolver can resolve a single query.
type QueryResolver interface {
	Resolve(ctx context.Context, query *schema.Field) *Resolved
}

// QueryResolverFunc is an adapter that allows to build a QueryResolver from
// a function.  Based on the http.HandlerFunc pattern.
type QueryResolverFunc func(ctx context.Context, query *schema.Field) *Resolved

// Resolve calls qr(ctx, query)
func (qr QueryResolverFunc) Resolve(ctx context.Context, query *schema.Field) *Resolved {
	return qr(ctx, query)
}

// NewQueryResolver creates a new query resolver.  The resolver runs the pipeline:
// 1) rewrite the query using qr (return error if failed)
// 2) execute the rewritten query with ex (return error if failed)
// 3) process the result with rc
func NewQueryResolver(qr *QueryRewriter, ex DgraphExecutor) QueryResolver {
	return &queryResolver{queryRewriter: qr, executor: ex, resultCompleter: CompletionFunc(noopCompletion)}
}

// NewEntitiesQueryResolver creates a new query resolver for `_entities` query.
// It is introduced because result completion works little different for `_entities` query.
func NewEntitiesQueryResolver(qr *QueryRewriter, ex DgraphExecutor) QueryResolver {
	return &queryResolver{queryRewriter: qr, executor: ex, resultCompleter: CompletionFunc(entitiesQueryCompletion)}
}

// a queryResolver can resolve a single GraphQL query field.
type queryResolver struct {
	queryRewriter   *QueryRewriter
	executor        DgraphExecutor
	resultCompleter CompletionFunc
}

func (qr *queryResolver) Resolve(ctx context.Context, query *schema.Field) *Resolved {
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveQuery")
	defer stop()

	resolverTrace := &schema.ResolverTrace{
		Path:       []interface{}{query.ResponseName()},
		ParentType: "Query",
		FieldName:  query.ResponseName(),
		ReturnType: query.Type().String(),
	}
	timer := newtimer(ctx, &resolverTrace.OffsetDuration)
	timer.Start()
	defer timer.Stop()

	resolved := qr.rewriteAndExecute(ctx, query)
	qr.resultCompleter.Complete(ctx, resolved)
	return resolved
}

func (qr *queryResolver) rewriteAndExecute(ctx context.Context, query *schema.Field) *Resolved {
	dgraphQueryDuration := &schema.LabeledOffsetDuration{Label: "query"}
	ext := &schema.Extensions{}

	emptyResult := func(err error) *Resolved {
		return &Resolved{
			// all the auto-generated queries are nullable, but users may define queries with
			// @custom(dql: ...) which may be non-nullable. So, we need to set the Data field
			// only if the query was nullable and keep it nil if it was non-nullable.
			// query.NullResponse() method handles that.
			Data:       query.NullResponse(),
			Field:      query,
			Err:        schema.SetPathIfEmpty(err, query.ResponseName()),
			Extensions: ext,
		}
	}

	dgQuery, err := qr.queryRewriter.Rewrite(ctx, query)
	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't rewrite query %s",
			query.ResponseName()))
	}
	qry := dgraph.AsString(dgQuery)
	glog.Infof("DQL Query: %s\n", qry)

	queryTimer := newtimer(ctx, &dgraphQueryDuration.OffsetDuration)
	queryTimer.Start()

	req := &edgraph.Request{
		Req:      &pb.Request{Query: qry, ReadOnly: true},
		GqlField: query,
	}
	resp, err := qr.executor.Execute(ctx, req)
	queryTimer.Stop()

	if err != nil && !x.IsGqlErrorList(err) {
		err = schema.GQLWrapf(err, "Dgraph query failed")
		glog.Infof("Dgraph query execution failed : %s", err)
	}

	ext.TouchedUids = resp.GetMetrics().GetNumUids()[touchedUidsKey]
	resolved := &Resolved{
		Data:       resp.GetJson(),
		Field:      query,
		Err:        schema.SetPathIfEmpty(err, query.ResponseName()),
		Extensions: ext,
	}

	return resolved
}

func NewCustomDQLQueryResolver(qr *QueryRewriter, ex DgraphExecutor) QueryResolver {
	return &customDQLQueryResolver{queryRewriter: qr, executor: ex}
}

type customDQLQueryResolver struct {
	queryRewriter *QueryRewriter
	executor      DgraphExecutor
}

func (qr *customDQLQueryResolver) Resolve(ctx context.Context, query *schema.Field) *Resolved {
	span := otrace.FromContext(ctx)
	stop := x.SpanTimer(span, "resolveCustomDQLQuery")
	defer stop()

	resolverTrace := &schema.ResolverTrace{
		Path:       []interface{}{query.ResponseName()},
		ParentType: "Query",
		FieldName:  query.ResponseName(),
		ReturnType: query.Type().String(),
	}
	timer := newtimer(ctx, &resolverTrace.OffsetDuration)
	timer.Start()
	defer timer.Stop()

	resolved := qr.rewriteAndExecute(ctx, query)
	return resolved
}

func (qr *customDQLQueryResolver) rewriteAndExecute(ctx context.Context,
	query *schema.Field) *Resolved {
	dgraphQueryDuration := &schema.LabeledOffsetDuration{Label: "query"}
	ext := &schema.Extensions{}

	emptyResult := func(err error) *Resolved {
		resolved := EmptyResult(query, err)
		resolved.Extensions = ext
		return resolved
	}

	vars, err := dqlVars(query.Arguments())
	if err != nil {
		return emptyResult(err)
	}

	dgQuery, err := qr.queryRewriter.Rewrite(ctx, query)
	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "got error while rewriting DQL query"))
	}

	qry := dgraph.AsString(dgQuery)

	queryTimer := newtimer(ctx, &dgraphQueryDuration.OffsetDuration)
	queryTimer.Start()

	req := &edgraph.Request{
		Req: &pb.Request{Query: qry, Vars: vars, ReadOnly: true},
	}
	resp, err := qr.executor.Execute(ctx, req)
	queryTimer.Stop()

	if err != nil {
		return emptyResult(schema.GQLWrapf(err, "Dgraph query failed"))
	}
	ext.TouchedUids = resp.GetMetrics().GetNumUids()[touchedUidsKey]

	var respJson map[string]interface{}
	if err = schema.Unmarshal(resp.Json, &respJson); err != nil {
		return emptyResult(schema.GQLWrapf(err, "couldn't unmarshal Dgraph result"))
	}

	resolved := DataResult(query, respJson, nil)
	resolved.Extensions = ext
	return resolved
}

func resolveIntrospection(ctx context.Context, q *schema.Field) *Resolved {
	data, err := schema.Introspect(q)
	return &Resolved{
		Data:  data,
		Field: q,
		Err:   err,
	}
}

// converts scalar values received from GraphQL arguments to go string
// If it is a scalar only possible cases are: string, bool, int64, float64 and nil.
func convertScalarToString(val interface{}) (string, error) {
	var str string
	switch v := val.(type) {
	case string:
		str = v
	case bool:
		str = strconv.FormatBool(v)
	case int64:
		str = strconv.FormatInt(v, 10)
	case float64:
		str = strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		str = v.String()
	case nil:
		str = ""
	default:
		return "", errNotScalar
	}
	return str, nil
}

func dqlVars(args map[string]interface{}) (map[string]string, error) {
	vars := make(map[string]string)
	for k, v := range args {
		// pb.Request{}.Vars accepts only string values for variables,
		// so need to convert all variable values to string
		vStr, err := convertScalarToString(v)
		if err != nil {
			return vars, schema.GQLWrapf(err, "couldn't convert argument %s to string", k)
		}
		// the keys in pb.Request{}.Vars are assumed to be prefixed with $
		vars["$"+k] = vStr
	}
	return vars, nil
}
