// Portions Copyright 2017-2018 Dgraph Labs, Inc. are available under the Apache License v2.0.
// Portions Copyright 2022 Outcaste LLC are available under the Sustainable License v1.0.

package boot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	farm "github.com/dgryski/go-farm"
	"github.com/golang/snappy"
	"github.com/outcaste-io/outserv/chunker"
	"github.com/outcaste-io/outserv/posting"
	"github.com/outcaste-io/outserv/protos/pb"
	"github.com/outcaste-io/outserv/tok"
	"github.com/outcaste-io/outserv/types"
	"github.com/outcaste-io/outserv/x"
	"github.com/outcaste-io/ristretto/z"
)

type mapper struct {
	*state
	shards []shardState // shard is based on predicate
}

type shardState struct {
	// Buffer up map entries until we have a sufficient amount, then sort and
	// write them to file.
	cbuf *z.Buffer
	mu   sync.Mutex // Allow only 1 write per shard at a time.
}

func newMapperBuffer(opt *options) *z.Buffer {
	sz := float64(opt.MapBufSize) * 1.1
	tmpDir := filepath.Join(opt.TmpDir, bufferDir)
	buf, err := z.NewBufferTmp(tmpDir, int(sz))
	x.Check(err)
	return buf.WithMaxSize(2 * int(opt.MapBufSize))
}

func newMapper(st *state) *mapper {
	shards := make([]shardState, st.opt.MapShards)
	for i := range shards {
		shards[i].cbuf = newMapperBuffer(st.opt)
	}
	return &mapper{
		state:  st,
		shards: shards,
	}
}

type MapEntry []byte

// type mapEntry struct {
// 	uid   uint64 // if plist is filled, then corresponds to plist's uid.
// 	key   []byte
// 	plist []byte
// }

func mapEntrySize(key []byte, p *pb.Posting) int {
	return 8 + 4 + 4 + len(key) + p.Size() // UID + keySz + postingSz + len(key) + size(p)
}

func marshalMapEntry(dst []byte, uid uint64, key []byte, p *pb.Posting) {
	if p != nil {
		uid = p.Uid
	}
	binary.BigEndian.PutUint64(dst[0:8], uid)
	binary.BigEndian.PutUint32(dst[8:12], uint32(len(key)))

	psz := p.Size()
	binary.BigEndian.PutUint32(dst[12:16], uint32(psz))

	n := copy(dst[16:], key)

	if psz > 0 {
		pbuf := dst[16+n:]
		_, err := p.MarshalToSizedBuffer(pbuf[:psz])
		x.Check(err)
	}

	x.AssertTrue(len(dst) == 16+n+psz)
}

func (me MapEntry) Size() int {
	return len(me)
}

func (me MapEntry) Uid() uint64 {
	return binary.BigEndian.Uint64(me[0:8])
}

func (me MapEntry) Key() []byte {
	sz := binary.BigEndian.Uint32(me[8:12])
	return me[16 : 16+sz]
}

func (me MapEntry) Plist() []byte {
	ksz := binary.BigEndian.Uint32(me[8:12])
	sz := binary.BigEndian.Uint32(me[12:16])
	start := 16 + ksz
	return me[start : start+sz]
}

func less(lhs, rhs MapEntry) bool {
	if keyCmp := bytes.Compare(lhs.Key(), rhs.Key()); keyCmp != 0 {
		return keyCmp < 0
	}
	return lhs.Uid() < rhs.Uid()
}

func (m *mapper) openOutputFile(shardIdx int) (*os.File, error) {
	fileNum := atomic.AddUint32(&m.mapFileId, 1)
	filename := filepath.Join(
		m.opt.TmpDir,
		mapShardDir,
		fmt.Sprintf("%03d", shardIdx),
		fmt.Sprintf("%06d.map.gz", fileNum),
	)
	x.Check(os.MkdirAll(filepath.Dir(filename), 0750))
	return os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
}

func (m *mapper) writeMapEntriesToFile(cbuf *z.Buffer, shardIdx int) {
	defer func() {
		m.shards[shardIdx].mu.Unlock() // Locked by caller.
		cbuf.Release()
	}()

	cbuf.SortSlice(func(ls, rs []byte) bool {
		lhs := MapEntry(ls)
		rhs := MapEntry(rs)
		return less(lhs, rhs)
	})

	f, err := m.openOutputFile(shardIdx)
	x.Check(err)

	defer func() {
		x.Check(f.Sync())
		x.Check(f.Close())
	}()

	w := snappy.NewBufferedWriter(f)
	defer func() {
		x.Check(w.Close())
	}()

	// Create partition keys for the map file.
	header := &pb.MapHeader{
		PartitionKeys: [][]byte{},
	}

	var bufSize int64
	cbuf.SliceIterate(func(slice []byte) error {
		me := MapEntry(slice)
		bufSize += int64(4 + len(me))
		if bufSize < m.opt.PartitionBufSize {
			return nil
		}
		sz := len(header.PartitionKeys)
		if sz > 0 && bytes.Equal(me.Key(), header.PartitionKeys[sz-1]) {
			// We already have this key.
			return nil
		}
		header.PartitionKeys = append(header.PartitionKeys, me.Key())
		bufSize = 0
		return nil
	})

	// Write the header to the map file.
	headerBuf, err := header.Marshal()
	x.Check(err)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(headerBuf)))
	x.Check2(w.Write(lenBuf))
	x.Check2(w.Write(headerBuf))
	x.Check(err)

	sizeBuf := make([]byte, binary.MaxVarintLen64)

	err = cbuf.SliceIterate(func(slice []byte) error {
		n := binary.PutUvarint(sizeBuf, uint64(len(slice)))
		_, err := w.Write(sizeBuf[:n])
		x.Check(err)

		_, err = w.Write(slice)
		return err
	})
	x.Check(err)
}

var once sync.Once

func (m *mapper) run() {
	chunk := chunker.NewChunker(chunker.JsonFormat, 1000)
	nquads := chunk.NQuads()
	go func() {
		for chunkBuf := range m.readerChunkCh {
			if err := chunk.Parse(chunkBuf); err != nil {
				atomic.AddInt64(&m.prog.errCount, 1)
				if !m.opt.IgnoreErrors {
					x.Check(err)
				}
			}
		}
		nquads.Flush()
	}()

	for nqs := range nquads.Ch() {
		for _, nq := range nqs {
			m.processNQuad(nq)
			atomic.AddInt64(&m.prog.nquadCount, 1)
		}

		for i := range m.shards {
			sh := &m.shards[i]
			if uint64(sh.cbuf.LenNoPadding()) >= m.opt.MapBufSize {
				sh.mu.Lock() // One write at a time.
				go m.writeMapEntriesToFile(sh.cbuf, i)
				// Clear the entries and encodedSize for the next batch.
				// Proactively allocate 32 slots to bootstrap the entries slice.
				sh.cbuf = newMapperBuffer(m.opt)
			}
		}
	}

	for i := range m.shards {
		sh := &m.shards[i]
		if sh.cbuf.LenNoPadding() > 0 {
			sh.mu.Lock() // One write at a time.
			m.writeMapEntriesToFile(sh.cbuf, i)
		} else {
			sh.cbuf.Release()
		}
		m.shards[i].mu.Lock() // Ensure that the last file write finishes.
	}
}

func (m *mapper) addMapEntry(key []byte, p *pb.Posting, shard int) {
	atomic.AddInt64(&m.prog.mapEdgeCount, 1)

	uid := p.Uid
	if p.PostingType != pb.Posting_REF {
		// Keep p
	} else {
		// We only needed the UID.
		p = nil
	}

	sh := &m.shards[shard]

	sz := mapEntrySize(key, p)
	dst := sh.cbuf.SliceAllocate(sz)
	marshalMapEntry(dst, uid, key, p)
}

func (m *mapper) processNQuad(nq *pb.Edge) {
	if m.opt.Namespace != math.MaxUint64 {
		// Use the specified namespace passed through '--force-namespace' flag.
		nq.Namespace = m.opt.Namespace
	}
	var typ string
	if strings.HasPrefix(nq.GetSubject(), "_:") {
		typ = strings.SplitN(nq.Subject[2:], ".", 2)[0]
	}

	sid := m.uid(nq.GetSubject(), nq.Namespace)
	if sid == 0 {
		panic(fmt.Sprintf("invalid UID with value 0 for %v", nq.GetSubject()))
	}
	var oid uint64
	var de *pb.Edge
	if nq.GetObjectValue() == nil {
		oid = m.uid(nq.GetObjectId(), nq.Namespace)
		if oid == 0 {
			panic(fmt.Sprintf("invalid UID with value 0 for %v", nq.GetObjectId()))
		}
		de = &pb.Edge{
			Subject:   x.ToHexString(sid),
			Predicate: typ + "." + nq.Predicate,
			ObjectId:  x.ToHexString(oid),
			Namespace: nq.Namespace,
		}
	} else {
		de = &pb.Edge{
			Subject:     x.ToHexString(sid),
			Predicate:   typ + "." + nq.Predicate,
			ObjectValue: nq.ObjectValue,
			Namespace:   nq.Namespace,
		}
	}

	m.dqlSchema.checkAndSetInitialSchema(nq.Namespace)

	// Appropriate schema must exist for the nquad's namespace by this time.
	de.Predicate = x.NamespaceAttr(de.Namespace, de.Predicate)

	createEntries := func(de *pb.Edge) {
		fwd := m.createPostings(de)
		shard := m.state.shards.shardFor(de.Predicate)
		key := x.DataKey(de.Predicate, x.FromHex(de.Subject))
		m.addMapEntry(key, fwd, shard)
		m.addIndexMapEntries(de)
	}

	createEntries(de)

	gqlType := m.gqlSchema.Type(typ)
	if fd := gqlType.Field(nq.Predicate); fd.Inverse() != nil {
		re := &pb.Edge{
			Subject:   x.ToHexString(oid),
			Predicate: x.NamespaceAttr(de.Namespace, fd.Inverse().DgraphPredicate()),
			ObjectId:  x.ToHexString(sid),
			Namespace: nq.Namespace,
		}
		createEntries(re)
	}
}

func (m *mapper) uid(xid string, ns uint64) uint64 {
	if uid, err := strconv.ParseUint(xid, 0, 64); err == nil {
		m.xids.BumpPast(uid)
		return uid
	}

	return m.lookupUid(xid, ns)
}

func (m *mapper) lookupUid(xid string, ns uint64) uint64 {
	// We create a copy of xid string here because it is stored in
	// the map in AssignUid and going to be around throughout the process.
	// We don't want to keep the whole line that we read from file alive.
	// xid is a substring of the line that we read from the file and if
	// xid is alive, the whole line is going to be alive and won't be GC'd.
	// Also, checked that sb goes on the stack whereas sb.String() goes on
	// heap. Note that the calls to the strings.Builder.* are inlined.

	// With Trie, we no longer need to use strings.Builder, because Trie would use its own storage
	// for the strings.
	// sb := strings.Builder{}
	// x.Check2(sb.WriteString(xid))
	// uid, isNew := m.xids.AssignUid(sb.String())

	// There might be a case where Nquad from different namespace have the same xid.
	uid, _ := m.xids.AssignUid(x.NamespaceAttr(ns, xid))
	return uid
}

func (m *mapper) createPostings(nq *pb.Edge) *pb.Posting {
	// 	m.schema.validateType(de, nq.ObjectValue == nil)
	// TODO: Understand the above.

	p, err := posting.NewPosting(nq)
	x.Check(err)

	sch := m.dqlSchema.getSchema(nq.Predicate)
	if sch == nil {
		fmt.Printf("schema: %+v\n", m.dqlSchema.schemaMap)
		fmt.Printf("asking for: %q\n", nq.Predicate)
		x.AssertTrue(sch != nil)
	}
	if nq.GetObjectValue() != nil {
		switch {
		// TODO(mrjn): We should stop assigning Uids to values.
		case sch.List:
			p.Uid = farm.Fingerprint64(nq.ObjectValue)
		default:
			p.Uid = math.MaxUint64
		}
	}

	// Early exit for no reverse edge.
	return p
}

func (m *mapper) addIndexMapEntries(nq *pb.Edge) {
	if nq.GetObjectValue() == nil {
		return // Cannot index UIDs
	}

	sch := m.dqlSchema.getSchema(nq.Predicate)
	for _, tokName := range sch.GetTokenizer() {
		// Find tokeniser.
		toker, ok := tok.GetTokenizer(tokName)
		if !ok {
			log.Fatalf("unknown tokenizer %q", tokName)
		}

		// Convert from storage type to schema type.
		schemaVal, err := types.Convert(nq.ObjectValue, types.TypeID(sch.GetValueType()))
		// Shouldn't error, since we've already checked for convertibility when
		// doing edge postings. So okay to be fatal.
		x.Check(err)

		// Extract tokens.
		toks, err := tok.BuildTokens(schemaVal.Value, toker)
		x.Check(err)

		// Store index posting.
		for _, t := range toks {
			m.addMapEntry(
				x.IndexKey(nq.Predicate, t),
				&pb.Posting{
					Uid:         x.FromHex(nq.Subject),
					PostingType: pb.Posting_REF,
				},
				m.state.shards.shardFor(nq.Predicate),
			)
		}
	}
}
