// Portions Copyright 2017-2018 Dgraph Labs, Inc. are available under the Apache License v2.0.
// Portions Copyright 2022 Outcaste LLC are available under the Sustainable License v1.0.

package worker

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"

	"github.com/outcaste-io/outserv/badger"
	bpb "github.com/outcaste-io/outserv/badger/pb"
	"github.com/outcaste-io/ristretto/z"

	"github.com/outcaste-io/outserv/ee/enc"
	"github.com/outcaste-io/outserv/posting"
	"github.com/outcaste-io/outserv/protos/pb"
	"github.com/outcaste-io/outserv/types"
	"github.com/outcaste-io/outserv/x"
)

// DefaultExportFormat stores the name of the default format for exports.
const DefaultExportFormat = "json"

type exportFormat struct {
	ext  string // file extension
	pre  string // string to write before exported records
	post string // string to write after exported records
}

var exportFormats = map[string]exportFormat{
	"json": {
		ext:  ".json",
		pre:  "[\n",
		post: "\n]\n",
	},
}

type exporter struct {
	pl        *posting.List
	uid       uint64
	attr      string
	namespace uint64
	readTs    uint64
}

// UIDs like 0x1 look weird but 64-bit ones like 0x0000000000000001 are too long.
var uidFmtStrJson = "\"%#x\""

// valToStr converts a posting value to a string.
func valToStr(v types.Sval) (string, error) {
	v2, err := types.Convert(v, types.TypeString)
	if err != nil {
		return "", errors.Wrapf(err, "while converting %v to string", v2.Value)
	}

	// Strip terminating null, if any.
	return strings.TrimRight(v2.Value.(string), "\x00"), nil
}

// escapedString converts a string into an escaped string for exports.
func escapedString(str string) string {
	// We use the Marshal function in the JSON package for all export formats
	// because it properly escapes strings.
	byt, err := json.Marshal(str)
	if err != nil {
		// All valid stings should be able to be escaped to a JSON string so
		// it's safe to panic here. Marshal has to return an error because it
		// accepts an interface.
		x.Panic(errors.New("Could not marshal string to JSON string"))
	}
	return string(byt)
}

func (e *exporter) toJSON() (*bpb.KVList, error) {
	bp := new(bytes.Buffer)
	// We could output more compact JSON at the cost of code complexity.
	// Leaving it simple for now.

	continuing := false
	mapStart := fmt.Sprintf("  {\"uid\":"+uidFmtStrJson+`,"namespace":"%#x"`, e.uid, e.namespace)
	err := e.pl.IterateAll(e.readTs, 0, func(p *pb.Posting) error {
		if continuing {
			fmt.Fprint(bp, ",\n")
		} else {
			continuing = true
		}

		fmt.Fprint(bp, mapStart)
		if p.PostingType == pb.Posting_REF {
			fmt.Fprintf(bp, `,"%s":[`, e.attr)
			fmt.Fprintf(bp, "{\"uid\":"+uidFmtStrJson, p.Uid)
			fmt.Fprint(bp, "}]")
		} else {
			fmt.Fprintf(bp, `,"%s":`, e.attr)
			str, err := valToStr(types.Sval(p.Value))
			if err != nil {
				// Copying this behavior from RDF exporter.
				// TODO Investigate why returning here before before completely
				//      exporting this posting is not considered data loss.
				glog.Errorf("Ignoring error: %+v\n", err)
				return nil
			}

			if !types.TypeID(p.Value[0]).IsNumber() {
				str = escapedString(str)
			}

			fmt.Fprint(bp, str)
		}

		fmt.Fprint(bp, "}")
		return nil
	})

	kv := &bpb.KV{
		Value:   bp.Bytes(),
		Version: 1,
	}
	return listWrap(kv), err
}

func toSchema(attr string, update *pb.SchemaUpdate) *bpb.KV {
	// bytes.Buffer never returns error for any of the writes. So, we don't need to check them.
	ns, attr := x.ParseNamespaceAttr(attr)
	var buf bytes.Buffer
	x.Check2(buf.WriteString(fmt.Sprintf("[%#x]", ns)))
	x.Check2(buf.WriteRune(' '))
	x.Check2(buf.WriteRune('<'))
	x.Check2(buf.WriteString(attr))
	x.Check2(buf.WriteRune('>'))
	x.Check2(buf.WriteRune(':'))
	if update.GetList() {
		x.Check2(buf.WriteRune('['))
	}
	x.Check2(buf.WriteString(types.TypeID(update.GetValueType()).String()))
	if update.GetList() {
		x.Check2(buf.WriteRune(']'))
	}
	switch {
	case update.GetDirective() == pb.SchemaUpdate_INDEX && len(update.GetTokenizer()) > 0:
		x.Check2(fmt.Fprintf(&buf, " @index(%s)", strings.Join(update.GetTokenizer(), ",")))
	}
	if update.GetCount() {
		x.Check2(buf.WriteString(" @count"))
	}
	if update.GetUpsert() {
		x.Check2(buf.WriteString(" @upsert"))
	}
	x.Check2(buf.WriteString(" . \n"))
	//TODO(Naman): We don't need the version anymore.
	return &bpb.KV{
		Value:   buf.Bytes(),
		Version: 3, // Schema value
	}
}

type ExportWriter struct {
	w             io.WriteCloser
	bw            *bufio.Writer
	gw            *gzip.Writer
	relativePath  string
	hasDataBefore bool
}

func newExportWriter(handler x.UriHandler, fileName string) (*ExportWriter, error) {
	writer := &ExportWriter{relativePath: fileName}
	var err error

	writer.w, err = handler.CreateFile(fileName)
	if err != nil {
		return nil, err
	}
	writer.bw = bufio.NewWriterSize(writer.w, 1e6)
	ew, err := enc.GetWriter(x.WorkerConfig.EncryptionKey, writer.bw)
	if err != nil {
		return nil, err
	}
	writer.gw, err = gzip.NewWriterLevel(ew, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	return writer, nil
}

func (writer *ExportWriter) Close() error {
	if writer == nil {
		return nil
	}
	var err1, err2, err3 error
	if writer.gw != nil {
		err1 = writer.gw.Close()
	}
	if writer.bw != nil {
		err2 = writer.bw.Flush()
	}
	if writer.w != nil {
		err3 = writer.w.Close()
	}
	return x.MultiError(err1, err2, err3)
}

// ExportedFiles has the relative path of files that were written during export
type ExportedFiles []string

// export creates a export of data by exporting it as an JSON gzip.
func export(ctx context.Context, in *pb.ExportRequest) (ExportedFiles, error) {
	if in.GroupId != groups().groupId() {
		return nil, errors.Errorf("Export request group mismatch. Mine: %d. Requested: %d",
			groups().groupId(), in.GroupId)
	}
	glog.Infof("Export requested at %d for namespace %d.", in.ReadTs, in.Namespace)

	// Let's wait for this server to catch up to all the updates until this ts.
	if err := posting.Oracle().WaitForTs(ctx, in.ReadTs); err != nil {
		return nil, err
	}
	glog.Infof("Running export for group %d at timestamp %d.", in.GroupId, in.ReadTs)

	return exportInternal(ctx, in, pstore, false)
}

func ToExportKvList(pk x.ParsedKey, pl *posting.List, in *pb.ExportRequest) (*bpb.KVList, error) {
	e := &exporter{
		readTs:    in.ReadTs,
		uid:       pk.Uid,
		namespace: x.ParseNamespace(pk.Attr),
		attr:      x.ParseAttr(pk.Attr),
		pl:        pl,
	}

	emptyList := &bpb.KVList{}
	switch {
	// These predicates are not required in the export data.
	case e.attr == "dgraph.graphql.xid":
	case e.attr == "dgraph.drop.op":
	case e.attr == "dgraph.graphql.p_query":

	case pk.IsData() && e.attr == "dgraph.graphql.schema":
		// Export the graphql schema.
		vals, err := pl.AllValues(in.ReadTs)
		if err != nil {
			return emptyList, errors.Wrapf(err, "cannot read value of GraphQL schema")
		}
		// if the GraphQL schema node was deleted with S * * delete mutation,
		// then the data key will be overwritten with nil value.
		// So, just skip exporting it as there will be no value for this data key.
		if len(vals) == 0 {
			return emptyList, nil
		}
		// Give an error only if we find more than one value for the schema.
		if len(vals) > 1 {
			return emptyList, errors.Errorf("found multiple values for the GraphQL schema")
		}

		schema, script := ParseAsSchemaAndScript(vals[0])
		exported := x.ExportedGQLSchema{
			Namespace: e.namespace,
			Schema:    schema,
			Script:    script,
		}
		data, err := json.Marshal(exported)
		if err != nil {
			return emptyList, errors.Wrapf(err, "Error marshalling GraphQL schema to json")
		}
		kv := &bpb.KV{
			Value:   data,
			Version: 2, // GraphQL schema value
		}
		return listWrap(kv), nil

	// below predicates no longer exist internally starting v21.03 but leaving them here
	// so that users with a binary with version >= 21.03 can export data from a version < 21.03
	// without this internal data showing up.
	case e.attr == "dgraph.cors":
	case e.attr == "dgraph.graphql.schema_created_at":
	case e.attr == "dgraph.graphql.schema_history":
	case e.attr == "dgraph.graphql.p_sha256hash":

	case pk.IsData():
		// The GraphQL layer will create a node of type "dgraph.graphql". That entry
		// should not be exported.
		if e.attr == "dgraph.type" {
			vals, err := e.pl.AllValues(in.ReadTs)
			if err != nil {
				return emptyList, errors.Wrapf(err, "cannot read value of dgraph.type entry")
			}
			if len(vals) == 1 {
				val := vals[0]
				if len(val) == 0 {
					return emptyList, errors.Errorf("cannot read value of dgraph.type entry")
				}
				if string(val[1:]) == "dgraph.graphql" {
					return emptyList, nil
				}
			}
		}

		switch in.Format {
		case "json":
			return e.toJSON()
		default:
			glog.Fatalf("Invalid export format found: %s", in.Format)
		}

	default:
		glog.Fatalf("Invalid key found: %+v %v\n", pk, hex.Dump([]byte(pk.Attr)))
	}
	return emptyList, nil
}

func WriteExport(writers *Writers, kv *bpb.KV, format string) error {
	// Skip nodes that have no data. Otherwise, the exported data could have
	// formatting and/or syntax errors.
	if len(kv.Value) == 0 {
		return nil
	}

	var dataSeparator []byte
	switch format {
	case "json":
		dataSeparator = []byte(",\n")
	default:
		glog.Fatalf("Invalid export format found: %s", format)
	}

	var writer *ExportWriter
	var sep []byte
	switch kv.Version {
	case 1: // data
		writer = writers.DataWriter
		sep = dataSeparator
	case 2: // graphQL schema
		writer = writers.GqlSchemaWriter
		sep = []byte(",\n") // use json separator.
	case 3: // graphQL schema
		writer = writers.SchemaWriter
	default:
		glog.Fatalf("Invalid data type found: %x", kv.Key)
	}

	if writer.hasDataBefore {
		if _, err := writer.gw.Write(sep); err != nil {
			return err
		}
	}
	// change the hasDataBefore flag so that the next data entry will have a separator
	// prepended
	writer.hasDataBefore = true

	_, err := writer.gw.Write(kv.Value)
	return err
}

type Writers struct {
	DataWriter      *ExportWriter
	SchemaWriter    *ExportWriter
	GqlSchemaWriter *ExportWriter
	closeOnce       sync.Once
}

var _ io.Closer = &Writers{}

func NewWriters(req *pb.ExportRequest) (*Writers, error) {
	// Create a UriHandler for the given destination.
	destination := req.GetDestination()
	if destination == "" {
		destination = x.WorkerConfig.Dir.Export
	}
	uri, err := url.Parse(destination)
	if err != nil {
		return nil, err
	}
	creds := &x.MinioCredentials{
		AccessKey:    req.GetAccessKey(),
		SecretKey:    req.GetSecretKey(),
		SessionToken: req.GetSessionToken(),
		Anonymous:    req.GetAnonymous(),
	}
	handler, err := x.NewUriHandler(uri, creds)
	if err != nil {
		return nil, err
	}

	// Create the export directory.
	if !handler.DirExists(".") {
		if err := handler.CreateDir("."); err != nil {
			return nil, errors.Wrap(err, "while creating export directory")
		}
	}
	uts := time.Unix(req.UnixTs, 0).UTC().Format("0102.1504")
	dirName := fmt.Sprintf("dgraph.r%d.u%s", req.ReadTs, uts)
	if err := handler.CreateDir(dirName); err != nil {
		return nil, errors.Wrap(err, "while creating export directory")
	}

	// Create writers for each export file.
	writers := &Writers{}
	newWriter := func(ext string) (*ExportWriter, error) {
		fileName := filepath.Join(dirName, fmt.Sprintf("g%02d%s", req.GroupId, ext))
		return newExportWriter(handler, fileName)
	}
	if writers.DataWriter, err = newWriter(exportFormats[req.Format].ext + ".gz"); err != nil {
		return writers, err
	}
	if writers.SchemaWriter, err = newWriter(".schema.gz"); err != nil {
		return writers, err
	}
	if writers.GqlSchemaWriter, err = newWriter(".gql_schema.gz"); err != nil {
		return writers, err
	}

	return writers, nil
}

// Closes the underlying writers.
// This may be called multiple times.
func (w *Writers) Close() error {
	if w == nil {
		return nil
	}
	var err1, err2, err3 error
	w.closeOnce.Do(func() {
		err1 = w.DataWriter.Close()
		err2 = w.SchemaWriter.Close()
		err3 = w.GqlSchemaWriter.Close()
	})
	return x.MultiError(err1, err2, err3)
}

// exportInternal contains the core logic to export a Dgraph database. If skipZero is set to
// false, the parts of this method that require to talk to zero will be skipped. This is useful
// when exporting a p directory directly from disk without a running cluster.
// It uses stream framework to export the data. While it uses an iterator for exporting the schema
// and types.
func exportInternal(ctx context.Context, in *pb.ExportRequest, db *badger.DB,
	skipZero bool) (ExportedFiles, error) {
	writers, err := NewWriters(in)
	defer writers.Close()
	if err != nil {
		return nil, err
	}

	// This stream exports only the data and the graphQL schema.
	stream := db.NewStreamAt(in.ReadTs)
	stream.Prefix = []byte{x.DefaultPrefix}
	if in.Namespace != math.MaxUint64 {
		// Export a specific namespace.
		stream.Prefix = append(stream.Prefix, x.NamespaceToBytes(in.Namespace)...)
	}
	stream.LogPrefix = "Export"
	stream.ChooseKey = func(item *badger.Item) bool {
		// Skip exporting delete data including Schema and Types.
		if item.IsDeletedOrExpired() {
			return false
		}
		pk, err := x.Parse(item.Key())
		if err != nil {
			glog.Errorf("error %v while parsing key %v during export. Skip.", err,
				hex.EncodeToString(item.Key()))
			return false
		}

		// Do not pick keys storing parts of a multi-part list. They will be read
		// from the main key.
		if pk.HasStartUid {
			return false
		}
		// _predicate_ is deprecated but leaving this here so that users with a
		// binary with version >= 1.1 can export data from a version < 1.1 without
		// this internal data showing up.
		if pk.Attr == "_predicate_" {
			return false
		}

		if !skipZero {
			if servesTablet, err := groups().ServesTablet(pk.Attr); err != nil || !servesTablet {
				return false
			}
		}
		return pk.IsData()
	}

	stream.KeyToList = func(key []byte, itr *badger.Iterator) (*bpb.KVList, error) {
		item := itr.Item()
		pk, err := x.Parse(item.Key())
		if err != nil {
			glog.Errorf("error %v while parsing key %v during export. Skip.", err,
				hex.EncodeToString(item.Key()))
			return nil, err
		}
		pl, err := posting.ReadPostingList(key, itr)
		if err != nil {
			return nil, errors.Wrapf(err, "cannot read posting list")
		}
		return ToExportKvList(pk, pl, in)
	}

	stream.Send = func(buf *z.Buffer) error {
		kv := &bpb.KV{}
		return buf.SliceIterate(func(s []byte) error {
			kv.Reset()
			if err := kv.Unmarshal(s); err != nil {
				return err
			}
			return WriteExport(writers, kv, in.Format)
		})
	}

	// This is used to export the schema and types.
	writePrefix := func(prefix byte) error {
		txn := db.NewReadTxn(in.ReadTs)
		defer txn.Discard()
		// We don't need to iterate over all versions.
		iopts := badger.DefaultIteratorOptions
		iopts.Prefix = []byte{prefix}
		if in.Namespace != math.MaxUint64 {
			iopts.Prefix = append(iopts.Prefix, x.NamespaceToBytes(in.Namespace)...)
		}

		itr := txn.NewIterator(iopts)
		defer itr.Close()
		for itr.Rewind(); itr.Valid(); itr.Next() {
			item := itr.Item()
			// Don't export deleted items.
			if item.IsDeletedOrExpired() {
				continue
			}
			pk, err := x.Parse(item.Key())
			if err != nil {
				glog.Errorf("error %v while parsing key %v during export. Skip.", err,
					hex.EncodeToString(item.Key()))
				return err
			}

			val, err := item.ValueCopy(nil)
			if err != nil {
				return errors.Wrap(err, "writePrefix failed to get value")
			}
			var kv *bpb.KV
			switch prefix {
			case x.ByteSchema:
				kv, err = SchemaExportKv(pk.Attr, val, skipZero)
				if err != nil {
					// Let's not propagate this error. We just log this and continue onwards.
					glog.Errorf("Unable to export schema: %+v. Err=%v\n", pk, err)
					continue
				}
			default:
				glog.Fatalf("Unhandled byte prefix: %v", prefix)
			}

			// Write to the appropriate writer.
			if _, err := writers.SchemaWriter.gw.Write(kv.Value); err != nil {
				return err
			}
		}
		return nil
	}
	xfmt := exportFormats[in.Format]

	// All prepwork done. Time to roll.
	if _, err = writers.GqlSchemaWriter.gw.Write([]byte(exportFormats["json"].pre)); err != nil {
		return nil, err
	}
	if _, err = writers.DataWriter.gw.Write([]byte(xfmt.pre)); err != nil {
		return nil, err
	}
	if err := stream.Orchestrate(ctx); err != nil {
		return nil, err
	}
	if _, err = writers.DataWriter.gw.Write([]byte(xfmt.post)); err != nil {
		return nil, err
	}
	if _, err = writers.GqlSchemaWriter.gw.Write([]byte(exportFormats["json"].post)); err != nil {
		return nil, err
	}

	// Write the schema and types.
	if err := writePrefix(x.ByteSchema); err != nil {
		return nil, err
	}

	// Finish up export.
	if err := writers.Close(); err != nil {
		return nil, err
	}
	glog.Infof("Export DONE for group %d at timestamp %d.", in.GroupId, in.ReadTs)
	files := ExportedFiles{
		writers.DataWriter.relativePath,
		writers.SchemaWriter.relativePath,
		writers.GqlSchemaWriter.relativePath}
	return files, nil
}

func SchemaExportKv(attr string, val []byte, skipZero bool) (*bpb.KV, error) {
	if !skipZero {
		servesTablet, err := groups().ServesTablet(attr)
		if err != nil || !servesTablet {
			return nil, errors.Errorf("Tablet not found for attribute: %v", err)
		}
	}

	var update pb.SchemaUpdate
	if err := update.Unmarshal(val); err != nil {
		return nil, err
	}
	return toSchema(attr, &update), nil
}

// Export request is used to trigger exports for the request list of groups.
// If a server receives request to export a group that it doesn't handle, it would
// automatically relay that request to the server that it thinks should handle the request.
func (w *grpcWorker) Export(ctx context.Context, req *pb.ExportRequest) (*pb.ExportResponse, error) {
	glog.Infof("Received export request via Grpc: %+v\n", req)
	if ctx.Err() != nil {
		glog.Errorf("Context error during export: %v\n", ctx.Err())
		return nil, ctx.Err()
	}

	glog.Infof("Issuing export request...")
	files, err := export(ctx, req)
	if err != nil {
		glog.Errorf("While running export. Request: %+v. Error=%v\n", req, err)
		return nil, err
	}
	glog.Infof("Export request: %+v OK.\n", req)
	return &pb.ExportResponse{Msg: "SUCCESS", Files: files}, nil
}

func handleExportOverNetwork(ctx context.Context, in *pb.ExportRequest) (ExportedFiles, error) {
	if in.GroupId == groups().groupId() {
		return export(ctx, in)
	}

	pl := groups().Leader(in.GroupId)
	if pl == nil {
		return nil, errors.Errorf("Unable to find leader of group: %d\n", in.GroupId)
	}

	glog.Infof("Sending export request to group: %d, addr: %s\n", in.GroupId, pl.Addr)
	c := pb.NewWorkerClient(pl.Get())
	_, err := c.Export(ctx, in)
	if err != nil {
		glog.Errorf("Export error received from group: %d. Error: %v\n", in.GroupId, err)
	}
	return nil, err
}

// ExportOverNetwork sends export requests to all the known groups.
func ExportOverNetwork(ctx context.Context, input *pb.ExportRequest) (ExportedFiles, error) {
	// If we haven't even had a single membership update, don't run export.
	if err := x.HealthCheck(); err != nil {
		glog.Errorf("Rejecting export request due to health check error: %v\n", err)
		return nil, err
	}
	// Get ReadTs from zero and wait for stream to catch up.
	readTs := posting.ReadTimestamp()
	glog.Infof("Using readTs: %d\n", readTs)

	// Let's first collect all groups.
	gids := KnownGroups()
	glog.Infof("Requesting export for groups: %v\n", gids)

	type filesAndError struct {
		ExportedFiles
		error
	}
	ch := make(chan filesAndError, len(gids))
	for _, gid := range gids {
		go func(group uint32) {
			req := &pb.ExportRequest{
				GroupId:   group,
				ReadTs:    readTs,
				UnixTs:    time.Now().Unix(),
				Format:    input.Format,
				Namespace: input.Namespace,

				Destination:  input.Destination,
				AccessKey:    input.AccessKey,
				SecretKey:    input.SecretKey,
				SessionToken: input.SessionToken,
				Anonymous:    input.Anonymous,
			}
			files, err := handleExportOverNetwork(ctx, req)
			ch <- filesAndError{files, err}
		}(gid)
	}

	var allFiles ExportedFiles
	for i := 0; i < len(gids); i++ {
		pair := <-ch
		if pair.error != nil {
			rerr := errors.Wrapf(pair.error, "Export failed at readTs %d", readTs)
			glog.Errorln(rerr)
			return nil, rerr
		}
		allFiles = append(allFiles, pair.ExportedFiles...)
	}

	glog.Infof("Export at readTs %d DONE", readTs)
	return allFiles, nil
}

// NormalizeExportFormat returns the normalized string for the export format if it is valid, an
// empty string otherwise.
func NormalizeExportFormat(format string) string {
	format = strings.ToLower(format)
	if _, ok := exportFormats[format]; ok {
		return format
	}
	return ""
}
