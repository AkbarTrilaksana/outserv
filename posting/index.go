// Portions Copyright 2016-2018 Dgraph Labs, Inc. are available under the Apache License v2.0.
// Portions Copyright 2022 Outcaste LLC are available under the Sustainable License v1.0.

package posting

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	otrace "go.opencensus.io/trace"

	"github.com/outcaste-io/outserv/badger"
	"github.com/outcaste-io/outserv/badger/options"
	bpb "github.com/outcaste-io/outserv/badger/pb"
	"github.com/outcaste-io/outserv/protos/pb"
	"github.com/outcaste-io/outserv/schema"
	"github.com/outcaste-io/outserv/tok"
	"github.com/outcaste-io/outserv/types"
	"github.com/outcaste-io/outserv/x"
	"github.com/outcaste-io/ristretto/z"
)

var emptyCountParams countParams

type indexMutationInfo struct {
	tokenizers []tok.Tokenizer
	edge       *pb.Edge // Represents the original uid -> value edge.
	val        types.Sval
	op         pb.Edge_Op
}

// indexTokens return tokens, without the predicate prefix and
// index rune, for specific tokenizers.
func indexTokens(ctx context.Context, info *indexMutationInfo) ([]string, error) {
	attr := info.edge.Predicate
	schemaType, err := schema.State().TypeOf(attr)
	if err != nil || !schemaType.IsScalar() {
		return nil, errors.Errorf("Cannot index attribute %s of type object.", attr)
	}

	if !schema.State().IsIndexed(ctx, attr) {
		return nil, errors.Errorf("Attribute %s is not indexed.", attr)
	}
	sv, err := types.Convert(info.val, schemaType)
	if err != nil {
		return nil, err
	}

	var tokens []string
	for _, it := range info.tokenizers {
		toks, err := tok.BuildTokens(sv.Value, it)
		if err != nil {
			return tokens, err
		}
		tokens = append(tokens, toks...)
	}
	return tokens, nil
}

// addIndexMutations adds mutation(s) for a single term, to maintain the index,
// but only for the given tokenizers.
// TODO - See if we need to pass op as argument as t should already have Op.
func (txn *Txn) addIndexMutations(ctx context.Context, info *indexMutationInfo) error {
	if info.tokenizers == nil {
		info.tokenizers = schema.State().Tokenizer(ctx, info.edge.Predicate)
	}

	if uid := x.FromHex(info.edge.Subject); uid == 0 {
		return errors.New("invalid UID with value 0")
	}
	tokens, err := indexTokens(ctx, info)
	if err != nil {
		// This data is not indexable
		return err
	}

	// Create a value token -> uid edge.
	edge := &pb.Edge{
		ObjectId:  info.edge.Subject,
		Predicate: info.edge.Predicate,
		Op:        info.op,
	}

	for _, token := range tokens {
		if err := txn.addIndexMutation(ctx, edge, token); err != nil {
			return err
		}
	}
	return nil
}

func (txn *Txn) addIndexMutation(ctx context.Context, edge *pb.Edge, token string) error {
	key := x.IndexKey(edge.Predicate, token)
	plist, err := txn.cache.GetFromDelta(key)
	if err != nil {
		return err
	}

	x.AssertTrue(plist != nil)
	return plist.addMutation(ctx, txn, edge)
}

// countParams is sent to updateCount function. It is used to update the count index.
// It deletes the uid from the key corresponding to <attr, countBefore> and adds it
// to <attr, countAfter>.
type countParams struct {
	attr        string
	countBefore int
	countAfter  int
	entity      string
	reverse     bool
}

func (l *List) handleDeleteAll(ctx context.Context, edge *pb.Edge, txn *Txn) error {
	isIndexed := schema.State().IsIndexed(ctx, edge.Predicate)
	hasCount := schema.State().HasCount(ctx, edge.Predicate)
	// To calculate length of posting list. Used for deletion of count index.
	plen := l.Length(txn.ReadTs, 0)
	err := l.IterateAll(txn.ReadTs, 0, func(p *pb.Posting) error {
		switch {
		case isIndexed:
			// Delete index edge of each posting.
			return txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers: schema.State().Tokenizer(ctx, edge.Predicate),
				edge:       edge,
				val:        edge.ObjectValue,
				op:         pb.Edge_DEL,
			})
		default:
			return nil
		}
	})
	if err != nil {
		return err
	}
	if hasCount {
		// Delete uid from count index. Deletion of reverses is taken care by addReverseMutation
		// above.
		if err := txn.updateCount(ctx, countParams{
			attr:        edge.Predicate,
			countBefore: plen,
			countAfter:  0,
			entity:      edge.Subject,
		}); err != nil {
			return err
		}
	}

	return l.addMutation(ctx, txn, edge)
}

func (txn *Txn) addCountMutation(ctx context.Context, t *pb.Edge, count uint32,
	reverse bool) error {
	key := x.CountKey(t.Predicate, count)
	plist, err := txn.cache.GetFromDelta(key)
	if err != nil {
		return err
	}

	x.AssertTruef(plist != nil, "plist is nil [%s] %d", t.Predicate, t.ObjectId)
	return plist.addMutation(ctx, txn, t)
}

func (txn *Txn) updateCount(ctx context.Context, params countParams) error {
	edge := pb.Edge{
		ObjectId:  params.entity,
		Predicate: params.attr,
		Op:        pb.Edge_DEL,
	}
	if params.countBefore > 0 {
		if err := txn.addCountMutation(ctx, &edge, uint32(params.countBefore),
			params.reverse); err != nil {
			return err
		}
	}

	if params.countAfter > 0 {
		edge.Op = pb.Edge_SET
		if err := txn.addCountMutation(ctx, &edge, uint32(params.countAfter),
			params.reverse); err != nil {
			return err
		}
	}
	return nil
}

func countAfterMutation(countBefore int, found bool, op pb.Edge_Op) int {
	if !found && op == pb.Edge_SET {
		return countBefore + 1
	} else if found && op == pb.Edge_DEL {
		return countBefore - 1
	}

	// Only conditions remaining are below, for which countAfter will be same as countBefore.
	// (found && op == pb.DirectedEdge_SET) || (!found && op == pb.DirectedEdge_DEL)
	return countBefore
}

func (txn *Txn) addMutationHelper(ctx context.Context, l *List, doUpdateIndex bool,
	hasCountIndex bool, t *pb.Edge) (types.Sval, bool, countParams, error) {

	t1 := time.Now()
	l.Lock()
	defer l.Unlock()

	if dur := time.Since(t1); dur > time.Millisecond {
		span := otrace.FromContext(ctx)
		span.Annotatef([]otrace.Attribute{otrace.BoolAttribute("slow-lock", true)},
			"Acquired lock %v %v %v", dur, t.Predicate, t.Subject)
	}

	getUID := func(t *pb.Edge) uint64 {
		if len(t.ObjectId) > 0 {
			return x.FromHex(t.ObjectId)
		}
		return fingerprintEdge(t)
	}

	// For countIndex we need to check if some posting already exists for uid and length of posting
	// list, hence will are calling l.getPostingAndLength(). If doUpdateIndex or delNonListPredicate
	// is true, we just need to get the posting for uid, hence calling l.findPosting().
	countBefore, countAfter := 0, 0
	var currPost *pb.Posting
	var found bool
	var err error

	delNonListPredicate := !schema.State().IsList(t.Predicate) &&
		t.Op == pb.Edge_DEL && !x.IsStarAll(t.ObjectValue)

	switch {
	case hasCountIndex:
		countBefore, found, currPost = l.getPostingAndLength(txn.ReadTs, 0, getUID(t))
		if countBefore == -1 {
			return nil, false, emptyCountParams, ErrTsTooOld
		}
	case doUpdateIndex || delNonListPredicate:
		found, currPost, err = l.findPosting(txn.ReadTs, fingerprintEdge(t))
		if err != nil {
			return nil, found, emptyCountParams, err
		}
	}

	// If the predicate schema is not a list, ignore delete triples whose object is not a star or
	// a value that does not match the existing value.
	if delNonListPredicate {
		newPost, err := NewPosting(t)
		if err != nil {
			return nil, found, emptyCountParams, err
		}

		// This is a scalar value of non-list type and a delete edge mutation, so if the value
		// given by the user doesn't match the value we have, we return found to be false, to avoid
		// deleting the uid from index posting list.
		// This second check is required because we fingerprint the scalar values as math.MaxUint64,
		// so even though they might be different the check in the doUpdateIndex block above would
		// return found to be true.
		if found && !(bytes.Equal(currPost.Value, newPost.Value) &&
			types.TypeID(currPost.Value[0]) == types.TypeID(newPost.Value[0])) {
			return nil, false, emptyCountParams, nil
		}
	}

	if err = l.addMutationInternal(ctx, txn, t); err != nil {
		return nil, found, emptyCountParams, err
	}

	var val types.Sval
	if found && doUpdateIndex {
		val = currPost.Value
	}

	if hasCountIndex {
		countAfter = countAfterMutation(countBefore, found, t.Op)
		return val, found, countParams{
			attr:        t.Predicate,
			countBefore: countBefore,
			countAfter:  countAfter,
			entity:      t.Subject,
		}, nil
	}
	return val, found, emptyCountParams, nil
}

// AddMutationWithIndex is addMutation with support for indexing. It also
// supports reverse edges.
func (l *List) AddMutationWithIndex(ctx context.Context, edge *pb.Edge, txn *Txn) error {
	if edge.Predicate == "" {
		return errors.Errorf("Predicate cannot be empty for edge with subject: [%v], object: [%v]"+
			" and value: [%v]", edge.Subject, edge.ObjectId, edge.ObjectValue)
	}

	if edge.Op == pb.Edge_DEL && x.IsStarAll(edge.ObjectValue) {
		return l.handleDeleteAll(ctx, edge, txn)
	}

	doUpdateIndex := pstore != nil && schema.State().IsIndexed(ctx, edge.Predicate)
	hasCountIndex := schema.State().HasCount(ctx, edge.Predicate)

	val, found, cp, err := txn.addMutationHelper(ctx, l, doUpdateIndex, hasCountIndex, edge)
	if err != nil {
		return err
	}
	if hasCountIndex && cp.countAfter != cp.countBefore {
		if err := txn.updateCount(ctx, cp); err != nil {
			return err
		}
	}
	if doUpdateIndex {
		// Exact matches.
		if found && len(val) > 0 {
			if err := txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers: schema.State().Tokenizer(ctx, edge.Predicate),
				edge:       edge,
				val:        val,
				op:         pb.Edge_DEL,
			}); err != nil {
				return err
			}
		}
		if edge.Op == pb.Edge_SET {
			if err := txn.addIndexMutations(ctx, &indexMutationInfo{
				tokenizers: schema.State().Tokenizer(ctx, edge.Predicate),
				edge:       edge,
				val:        edge.ObjectValue,
				op:         pb.Edge_SET,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// prefixesToDeleteTokensFor returns the prefixes to be deleted for index for the given attribute and token.
func prefixesToDeleteTokensFor(attr, tokenizerName string, hasLang bool) ([][]byte, error) {
	prefixes := [][]byte{}
	pk := x.ParsedKey{Attr: attr}
	prefix := pk.IndexPrefix()
	tokenizer, ok := tok.GetTokenizer(tokenizerName)
	if !ok {
		return nil, errors.Errorf("Could not find valid tokenizer for %s", tokenizerName)
	}
	prefix = append(prefix, tokenizer.Identifier())
	prefixes = append(prefixes, prefix)
	// All the parts of any list that has been split into multiple parts.
	// Such keys have a different prefix (the last byte is set to 1).
	prefix = pk.IndexPrefix()
	prefix[0] = x.ByteSplit
	prefix = append(prefix, tokenizer.Identifier())
	prefixes = append(prefixes, prefix)

	return prefixes, nil
}

// rebuilder handles the process of rebuilding an index.
type rebuilder struct {
	attr    string
	prefix  []byte
	startTs uint64

	// The posting list passed here is the on disk version. It is not coming
	// from the LRU cache.
	fn func(uid uint64, pl *List, txn *Txn) error
}

func (r *rebuilder) Run(ctx context.Context) error {
	if r.startTs == 0 {
		glog.Infof("maxassigned is 0, no indexing work for predicate %s", r.attr)
		return nil
	}

	// We write the index in a temporary badger first and then,
	// merge entries before writing them to p directory.
	tmpIndexDir, err := ioutil.TempDir(x.WorkerConfig.Dir.Tmp, "dgraph_index_")
	if err != nil {
		return errors.Wrap(err, "error creating temp dir for reindexing")
	}
	defer os.RemoveAll(tmpIndexDir)
	glog.V(1).Infof("Rebuilding indexes using the temp folder %s\n", tmpIndexDir)

	dbOpts := badger.DefaultOptions(tmpIndexDir).
		WithNumVersionsToKeep(math.MaxInt32).
		WithLogger(&x.ToGlog{}).
		WithCompression(options.None).
		WithLoggingLevel(badger.WARNING).
		WithMetricsEnabled(false)

	// Set cache if we have encryption.
	if len(x.WorkerConfig.EncryptionKey) > 0 {
		dbOpts.EncryptionKey = x.WorkerConfig.EncryptionKey
		dbOpts.BlockCacheSize = 100 << 20
		dbOpts.IndexCacheSize = 100 << 20
	}
	tmpDB, err := badger.Open(dbOpts)
	if err != nil {
		return errors.Wrap(err, "error opening temp badger for reindexing")
	}
	defer tmpDB.Close()

	glog.V(1).Infof(
		"Rebuilding index for predicate %s: Starting process. StartTs=%d. Prefix=\n%s\n",
		r.attr, r.startTs, hex.Dump(r.prefix))

	// Counter is used here to ensure that all keys are committed at different timestamp.
	// We set it to 1 in case there are no keys found and NewStreamAt is called with ts=0.
	var counter uint64 = 1

	tmpWriter := tmpDB.NewWriteBatch()
	stream := pstore.NewStreamAt(r.startTs)
	stream.LogPrefix = fmt.Sprintf("Rebuilding index for predicate %s (1/2):", r.attr)
	stream.Prefix = r.prefix
	stream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		// We should return quickly if the context is no longer valid.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		pk, err := x.Parse(key)
		if err != nil {
			return nil, errors.Wrapf(err, "could not parse key %s", hex.Dump(key))
		}

		l, err := ReadPostingList(key, itr)
		if err != nil {
			return nil, errors.Wrapf(err, "error reading posting list from disk")
		}

		// We are using different transactions in each call to KeyToList function. This could
		// be a problem for computing reverse count indexes if deltas for same key are added
		// in different transactions. Such a case doesn't occur for now.
		txn := NewTxn(r.startTs, r.startTs)
		if err := r.fn(pk.Uid, l, txn); err != nil {
			return nil, err
		}

		// Convert data into deltas.
		txn.Update(ctx, nil)

		// txn.cache.Lock() is not required because we are the only one making changes to txn.
		kvs := make([]*bpb.KV, 0, len(txn.cache.deltas))
		for key, data := range txn.cache.deltas {
			version := atomic.AddUint64(&counter, 1)
			kv := bpb.KV{
				Key:      []byte(key),
				Value:    data,
				UserMeta: []byte{BitDeltaPosting},
				Version:  version,
			}
			kvs = append(kvs, &kv)
		}

		return &bpb.KVList{Kv: kvs}, nil
	}
	stream.Send = func(buf *z.Buffer) error {
		if err := tmpWriter.Write(buf); err != nil {
			return errors.Wrap(err, "error setting entries in temp badger")
		}

		return nil
	}

	start := time.Now()
	if err := stream.Orchestrate(ctx); err != nil {
		return err
	}
	if err := tmpWriter.Flush(); err != nil {
		return err
	}
	glog.V(1).Infof("Rebuilding index for predicate %s: building temp index took: %v\n",
		r.attr, time.Since(start))

	// Now we write all the created posting lists to disk.
	glog.V(1).Infof("Rebuilding index for predicate %s: writing index to badger", r.attr)
	start = time.Now()
	defer func() {
		glog.V(1).Infof("Rebuilding index for predicate %s: writing index took: %v\n",
			r.attr, time.Since(start))
	}()

	writer := pstore.NewWriteBatch()
	tmpStream := tmpDB.NewStreamAt(counter)
	tmpStream.LogPrefix = fmt.Sprintf("Rebuilding index for predicate %s (2/2):", r.attr)
	tmpStream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		l, err := ReadPostingList(key, itr)
		if err != nil {
			return nil, errors.Wrap(err, "error in reading posting list from pstore")
		}
		// No need to write a loop after ReadPostingList to skip unread entries
		// for a given key because we only wrote BitDeltaPosting to temp badger.

		kvs, err := l.Rollup(nil)
		if err != nil {
			return nil, err
		}

		return &bpb.KVList{Kv: kvs}, nil
	}
	tmpStream.Send = func(buf *z.Buffer) error {
		return buf.SliceIterate(func(slice []byte) error {
			kv := &bpb.KV{}
			if err := kv.Unmarshal(slice); err != nil {
				return err
			}
			if len(kv.Value) == 0 {
				return nil
			}

			// We choose to write the PL at r.startTs, so it won't be read by txns,
			// which occurred before this schema mutation.
			e := &badger.Entry{
				Key:      kv.Key,
				Value:    kv.Value,
				UserMeta: BitCompletePosting,
			}
			if err := writer.SetEntryAt(e.WithDiscard(), r.startTs); err != nil {
				return errors.Wrap(err, "error in writing index to pstore")
			}
			return nil
		})
	}

	if err := tmpStream.Orchestrate(ctx); err != nil {
		return err
	}
	glog.V(1).Infof("Rebuilding index for predicate %s: Flushing all writes.\n", r.attr)
	return writer.Flush()
}

// IndexRebuild holds the info needed to initiate a rebuilt of the indices.
type IndexRebuild struct {
	Attr          string
	StartTs       uint64
	OldSchema     *pb.SchemaUpdate
	CurrentSchema *pb.SchemaUpdate
}

type indexOp int

const (
	indexNoop    indexOp = iota // Index should be left alone.
	indexDelete          = iota // Index should be deleted.
	indexRebuild         = iota // Index should be deleted and rebuilt.
)

// GetQuerySchema returns the schema that can be served while indexes are getting built.
// Query schema is defined as current schema minus tokens to delete from current schema.
func (rb *IndexRebuild) GetQuerySchema() *pb.SchemaUpdate {
	// Copy the current schema.
	querySchema := *rb.CurrentSchema
	info := rb.needsTokIndexRebuild()

	// Compute old.Tokenizer minus info.tokenizersToDelete.
	interimTokenizers := make([]string, 0)
	for _, t1 := range rb.OldSchema.Tokenizer {
		found := false
		for _, t2 := range info.tokenizersToDelete {
			if t1 == t2 {
				found = true
				break
			}
		}
		if !found {
			interimTokenizers = append(interimTokenizers, t1)
		}
	}
	querySchema.Tokenizer = interimTokenizers

	if rb.needsCountIndexRebuild() == indexRebuild {
		querySchema.Count = false
	}
	return &querySchema
}

// DropIndexes drops the indexes that need to be rebuilt.
func (rb *IndexRebuild) DropIndexes(ctx context.Context) error {
	prefixes, err := prefixesForTokIndexes(ctx, rb)
	if err != nil {
		return err
	}
	prefixes = append(prefixes, prefixesToDropCountIndex(ctx, rb)...)
	glog.Infof("Deleting indexes for %s", rb.Attr)
	return pstore.DropPrefix(prefixes...)
}

// BuildData updates data.
func (rb *IndexRebuild) BuildData(ctx context.Context) error {
	return rebuildListType(ctx, rb)
}

// NeedIndexRebuild returns true if any of the tokenizer, reverse
// or count indexes need to be rebuilt.
func (rb *IndexRebuild) NeedIndexRebuild() bool {
	return rb.needsTokIndexRebuild().op == indexRebuild ||
		rb.needsCountIndexRebuild() == indexRebuild
}

// BuildIndexes builds indexes.
func (rb *IndexRebuild) BuildIndexes(ctx context.Context) error {
	if err := rebuildTokIndex(ctx, rb); err != nil {
		return err
	}
	return rebuildCountIndex(ctx, rb)
}

type indexRebuildInfo struct {
	op                  indexOp
	tokenizersToDelete  []string
	tokenizersToRebuild []string
}

func (rb *IndexRebuild) needsTokIndexRebuild() indexRebuildInfo {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If the old schema is nil, we can treat it as an empty schema. Copy it
	// first to avoid overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	currIndex := rb.CurrentSchema.Directive == pb.SchemaUpdate_INDEX
	prevIndex := old.Directive == pb.SchemaUpdate_INDEX

	// Index does not need to be rebuilt or deleted if the scheme directive
	// did not require an index before and now.
	if !currIndex && !prevIndex {
		return indexRebuildInfo{
			op: indexNoop,
		}
	}

	// Index only needs to be deleted if the schema directive changed and the
	// new directive does not require an index. Predicate is not checking
	// prevIndex since the previous if statement guarantees both values are
	// different.
	if !currIndex {
		return indexRebuildInfo{
			op:                 indexDelete,
			tokenizersToDelete: old.Tokenizer,
		}
	}

	// All tokenizers in the index need to be deleted and rebuilt if the value
	// types have changed.
	if currIndex && rb.CurrentSchema.ValueType != old.ValueType {
		return indexRebuildInfo{
			op:                  indexRebuild,
			tokenizersToDelete:  old.Tokenizer,
			tokenizersToRebuild: rb.CurrentSchema.Tokenizer,
		}
	}

	// Index needs to be rebuilt if the tokenizers have changed
	prevTokens := make(map[string]struct{})
	for _, t := range old.Tokenizer {
		prevTokens[t] = struct{}{}
	}
	currTokens := make(map[string]struct{})
	for _, t := range rb.CurrentSchema.Tokenizer {
		currTokens[t] = struct{}{}
	}

	newTokenizers, deletedTokenizers := x.Diff(currTokens, prevTokens)

	// If the tokenizers are the same, nothing needs to be done.
	if len(newTokenizers) == 0 && len(deletedTokenizers) == 0 {
		return indexRebuildInfo{
			op: indexNoop,
		}
	}

	return indexRebuildInfo{
		op:                  indexRebuild,
		tokenizersToDelete:  deletedTokenizers,
		tokenizersToRebuild: newTokenizers,
	}
}

func prefixesForTokIndexes(ctx context.Context, rb *IndexRebuild) ([][]byte, error) {
	rebuildInfo := rb.needsTokIndexRebuild()
	prefixes := [][]byte{}

	if rebuildInfo.op == indexNoop {
		return prefixes, nil
	}

	glog.Infof("Computing prefix index for attr %s and tokenizers %s", rb.Attr,
		rebuildInfo.tokenizersToDelete)
	for _, tokenizer := range rebuildInfo.tokenizersToDelete {
		prefixesNonLang, err := prefixesToDeleteTokensFor(rb.Attr, tokenizer, false)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefixesNonLang...)
		if tokenizer != "exact" {
			continue
		}
		prefixesWithLang, err := prefixesToDeleteTokensFor(rb.Attr, tokenizer, true)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefixesWithLang...)
	}

	glog.Infof("Deleting index for attr %s and tokenizers %s", rb.Attr,
		rebuildInfo.tokenizersToRebuild)
	// Before rebuilding, the existing index needs to be deleted.
	for _, tokenizer := range rebuildInfo.tokenizersToRebuild {
		prefixesNonLang, err := prefixesToDeleteTokensFor(rb.Attr, tokenizer, false)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefixesNonLang...)
		if tokenizer != "exact" {
			continue
		}
		prefixesWithLang, err := prefixesToDeleteTokensFor(rb.Attr, tokenizer, true)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefixesWithLang...)
	}

	return prefixes, nil
}

// rebuildTokIndex rebuilds index for a given attribute.
// We commit mutations with startTs and ignore the errors.
func rebuildTokIndex(ctx context.Context, rb *IndexRebuild) error {
	rebuildInfo := rb.needsTokIndexRebuild()
	if rebuildInfo.op != indexRebuild {
		return nil
	}

	// Exit early if there are no tokenizers to rebuild.
	if len(rebuildInfo.tokenizersToRebuild) == 0 {
		return nil
	}

	glog.Infof("Rebuilding index for attr %s and tokenizers %s", rb.Attr,
		rebuildInfo.tokenizersToRebuild)
	tokenizers, err := tok.GetTokenizers(rebuildInfo.tokenizersToRebuild)
	if err != nil {
		return err
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = func(uid uint64, pl *List, txn *Txn) error {
		edge := pb.Edge{Predicate: rb.Attr, Subject: x.ToHexString(uid)}
		return pl.Iterate(txn.ReadTs, 0, func(p *pb.Posting) error {
			// Add index entries based on p.
			for {
				err := txn.addIndexMutations(ctx, &indexMutationInfo{
					tokenizers: tokenizers,
					edge:       &edge,
					val:        p.Value,
					op:         pb.Edge_SET,
				})
				switch err {
				case ErrRetry:
					time.Sleep(10 * time.Millisecond)
				default:
					return err
				}
			}
		})
	}
	return builder.Run(ctx)
}

func (rb *IndexRebuild) needsCountIndexRebuild() indexOp {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	// If the old schema is nil, treat it as an empty schema. Copy it to avoid
	// overwriting it in rb.
	old := rb.OldSchema
	if old == nil {
		old = &pb.SchemaUpdate{}
	}

	// Do nothing if the schema directive did not change.
	if rb.CurrentSchema.Count == old.Count {
		return indexNoop

	}

	// If the new schema does not require an index, delete the current index.
	if !rb.CurrentSchema.Count {
		return indexDelete
	}

	// Otherwise, the index needs to be rebuilt.
	return indexRebuild
}

func prefixesToDropCountIndex(ctx context.Context, rb *IndexRebuild) [][]byte {
	// Exit early if indices do not need to be rebuilt.
	op := rb.needsCountIndexRebuild()

	if op == indexNoop {
		return nil
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	prefixes := append([][]byte{}, pk.CountPrefix())

	// All the parts of any list that has been split into multiple parts.
	// Such keys have a different prefix (the last byte is set to 1).
	countPrefix := pk.CountPrefix()
	countPrefix[0] = x.ByteSplit
	prefixes = append(prefixes, countPrefix)

	return prefixes
}

// rebuildCountIndex rebuilds the count index for a given attribute.
func rebuildCountIndex(ctx context.Context, rb *IndexRebuild) error {
	op := rb.needsCountIndexRebuild()
	if op != indexRebuild {
		return nil
	}

	glog.Infof("Rebuilding count index for %s", rb.Attr)
	var reverse bool
	fn := func(uid uint64, pl *List, txn *Txn) error {
		t := &pb.Edge{
			ObjectId:  x.ToHexString(uid),
			Predicate: rb.Attr,
			Op:        pb.Edge_SET,
		}
		sz := pl.Length(rb.StartTs, 0)
		if sz == -1 {
			return nil
		}
		for {
			err := txn.addCountMutation(ctx, t, uint32(sz), reverse)
			switch err {
			case ErrRetry:
				time.Sleep(10 * time.Millisecond)
			default:
				return err
			}
		}
	}

	// Create the forward index.
	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = fn
	return builder.Run(ctx)
}

// needsListTypeRebuild returns true if the schema changed from a scalar to a
// list. It returns true if the index can be left as is.
func (rb *IndexRebuild) needsListTypeRebuild() (bool, error) {
	x.AssertTruef(rb.CurrentSchema != nil, "Current schema cannot be nil.")

	if rb.OldSchema == nil {
		return false, nil
	}
	if rb.CurrentSchema.List && !rb.OldSchema.List {
		return true, nil
	}
	if rb.OldSchema.List && !rb.CurrentSchema.List {
		return false, errors.Errorf("Type can't be changed from list to scalar for attr: [%s]"+
			" without dropping it first.", x.ParseAttr(rb.CurrentSchema.Predicate))
	}

	return false, nil
}

// rebuildListType rebuilds the index when the schema is changed from scalar to list type.
// We need to fingerprint the values to get the new ValueId.
func rebuildListType(ctx context.Context, rb *IndexRebuild) error {
	if needsRebuild, err := rb.needsListTypeRebuild(); !needsRebuild || err != nil {
		return err
	}

	pk := x.ParsedKey{Attr: rb.Attr}
	builder := rebuilder{attr: rb.Attr, prefix: pk.DataPrefix(), startTs: rb.StartTs}
	builder.fn = func(uid uint64, pl *List, txn *Txn) error {
		var mpost *pb.Posting
		err := pl.IterateAll(txn.ReadTs, 0, func(p *pb.Posting) error {
			// We only want to modify the untagged value. There could be other values with a
			// lang tag.
			if p.Uid == math.MaxUint64 {
				mpost = p
			}
			return nil
		})
		if err != nil {
			return err
		}
		if mpost == nil {
			return nil
		}
		// Delete the old edge corresponding to ValueId math.MaxUint64
		t := &pb.Edge{
			ObjectId:  x.ToHexString(mpost.Uid),
			Predicate: rb.Attr,
			Op:        pb.Edge_DEL,
		}

		// Ensure that list is in the cache run by txn. Otherwise, nothing would
		// get updated.
		pl = txn.cache.SetIfAbsent(string(pl.key), pl)
		if err := pl.addMutation(ctx, txn, t); err != nil {
			return err
		}
		// Add the new edge with the fingerprinted value id.
		newEdge := &pb.Edge{
			Predicate:   rb.Attr,
			ObjectValue: mpost.Value,
			Op:          pb.Edge_SET,
		}
		return pl.addMutation(ctx, txn, newEdge)
	}
	return builder.Run(ctx)
}

// DeleteAll deletes all entries in the posting list.
func DeleteAll() error {
	ResetCache()
	return pstore.DropAll()
}

// DeleteData deletes all data for the namespace but leaves types and schema intact.
func DeleteData(ns uint64) error {
	ResetCache()
	prefix := make([]byte, 9)
	prefix[0] = x.DefaultPrefix
	binary.BigEndian.PutUint64(prefix[1:], ns)
	return pstore.DropPrefix(prefix)
}

// DeletePredicate deletes all entries and indices for a given predicate. The delete may be logical
// based on DB options set.
func DeletePredicate(ctx context.Context, attr string, ts uint64) error {
	glog.Infof("Dropping predicate: [%s]", attr)
	// TODO: We should only delete cache for certain keys, not all the keys.
	ResetCache()
	prefix := x.PredicatePrefix(attr)
	if err := pstore.DropPrefix(prefix); err != nil {
		return err
	}
	return schema.State().Delete(attr, ts)
}

// DeletePredicateBlocking deletes all entries and indices for a given predicate. It also blocks the
// writes.
func DeletePredicateBlocking(ctx context.Context, attr string, ts uint64) error {
	glog.Infof("Dropping predicate: [%s]", attr)
	// TODO: We should only delete cache for certain keys, not all the keys.
	ResetCache()
	prefix := x.PredicatePrefix(attr)
	if err := pstore.DropPrefixBlocking(prefix); err != nil {
		return err
	}
	return schema.State().Delete(attr, ts)
}

// DeleteNamespace bans the namespace and deletes its predicates/types from the schema.
func DeleteNamespace(ns uint64) error {
	// TODO: We should only delete cache for certain keys, not all the keys.
	ResetCache()
	schema.State().DeletePredsForNs(ns)
	return pstore.BanNamespace(ns)
}
