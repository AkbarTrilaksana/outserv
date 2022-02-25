// Portions Copyright 2017-2018 Dgraph Labs, Inc. are available under the Apache 2.0 license.
// Portions Copyright 2022 Outcaste, Inc. are available under the Smart License.

package worker

import (
	"math"
	"os"

	"github.com/golang/glog"
	"github.com/outcaste-io/badger/v3"
	"github.com/outcaste-io/outserv/raftwal"
	"github.com/outcaste-io/outserv/x"
	"github.com/outcaste-io/ristretto/z"
)

const (
	// NOTE: SuperFlag defaults must include every possible option that can be used. This way, if a
	//       user makes a typo while defining a SuperFlag we can catch it and fail right away rather
	//       than fail during runtime while trying to retrieve an option that isn't there.
	//
	//       For easy readability, keep the options without default values (if any) at the end of
	//       the *Defaults string. Also, since these strings are printed in --help text, avoid line
	//       breaks.
	AuditDefaults  = `compress=false; days=10; size=100; dir=; output=; encrypt-file=;`
	BadgerDefaults = `compression=snappy; numgoroutines=8;`
	CacheDefaults  = `size-mb=1024; percentage=50,30,20;`
	CDCDefaults    = `file=; kafka=; sasl_user=; sasl_password=; ca_cert=; client_cert=; ` +
		`client_key=; sasl-mechanism=PLAIN; tls=false;`
	GraphQLDefaults = `introspection=true; debug=false; extensions=true; poll-interval=1s; `
	LambdaDefaults  = `url=; num=1; port=20000; restart-after=30s; `
	LimitDefaults   = `mutations=allow; query-edge=1000000; normalize-node=10000; ` +
		`mutations-nquad=1000000; disallow-drop=false; query-timeout=0ms; txn-abort-after=5m;` +
		`max-pending-queries=64;  max-retries=-1; shared-instance=false; max-splits=1000`
	RaftDefaults = `learner=false; snapshot-after-entries=10000; ` +
		`snapshot-after-duration=30m; pending-proposals=256; idx=; group=;`
	SecurityDefaults   = `token=; whitelist=;`
	ZeroLimitsDefaults = `uid-lease=0; refill-interval=30s; disable-admin-http=false;`
)

// ServerState holds the state of the Dgraph server.
type ServerState struct {
	Pstore   *badger.DB
	WALstore *raftwal.DiskStorage
	gcCloser *z.Closer // closer for valueLogGC
}

// State is the instance of ServerState used by the current server.
var State ServerState

// InitServerState initializes this server's state.
func InitServerState() {
	Config.validate()
	State.initStorage()

	groupId, err := x.ReadGroupIdFile(Config.PostingDir)
	if err != nil {
		glog.Warningf("Could not read %s file inside posting directory %s.", x.GroupIdFileName,
			Config.PostingDir)
	}
	x.WorkerConfig.ProposedGroupId = groupId
}

func setBadgerOptions(opt badger.Options) badger.Options {
	opt = opt.WithSyncWrites(false).
		WithLogger(&x.ToGlog{}).
		WithEncryptionKey(x.WorkerConfig.EncryptionKey)

	// Disable conflict detection in badger. Alpha runs in managed mode and
	// perform its own conflict detection so we don't need badger's conflict
	// detection. Using badger's conflict detection uses memory which can be
	// saved by disabling it.
	opt.DetectConflicts = false

	// Settings for the data directory.
	return opt
}

func (s *ServerState) initStorage() {
	var err error

	if x.WorkerConfig.EncryptionKey != nil {
		// non-nil key file
		if !EnterpriseEnabled() {
			// not licensed --> crash.
			glog.Fatal("Valid Enterprise License needed for the Encryption feature.")
		} else {
			// licensed --> OK.
			glog.Infof("Encryption feature enabled.")
		}
	}

	{
		// Write Ahead Log directory
		x.Checkf(os.MkdirAll(Config.WALDir, 0700), "Error while creating WAL dir.")
		s.WALstore, err = raftwal.InitEncrypted(Config.WALDir, x.WorkerConfig.EncryptionKey)
		x.Check(err)
	}
	{
		// Postings directory
		// All the writes to posting store should be synchronous. We use batched writers
		// for posting lists, so the cost of sync writes is amortized.
		x.Check(os.MkdirAll(Config.PostingDir, 0700))
		opt := x.WorkerConfig.Badger.
			WithDir(Config.PostingDir).WithValueDir(Config.PostingDir).
			WithNumVersionsToKeep(math.MaxInt32).
			WithNamespaceOffset(x.NamespaceOffset).
			WithExternalMagic(x.MagicVersion)
		opt = setBadgerOptions(opt)

		// Print the options w/o exposing key.
		// TODO: Build a stringify interface in Badger options, which is used to print nicely here.
		key := opt.EncryptionKey
		opt.EncryptionKey = nil
		glog.Infof("Opening postings BadgerDB with options: %+v\n", opt)
		opt.EncryptionKey = key

		s.Pstore, err = badger.OpenManaged(opt)
		x.Checkf(err, "Error while creating badger KV posting store")

		// zero out from memory
		opt.EncryptionKey = nil
	}
	// Temp directory
	x.Check(os.MkdirAll(x.WorkerConfig.TmpDir, 0700))

	s.gcCloser = z.NewCloser(3)
	go x.RunVlogGC(s.Pstore, s.gcCloser)
	// Commenting this out because Badger is doing its own cache checks.
	go x.MonitorCacheHealth(s.Pstore, s.gcCloser)
	go x.MonitorDiskMetrics("postings_fs", Config.PostingDir, s.gcCloser)
}

// Dispose stops and closes all the resources inside the server state.
func (s *ServerState) Dispose() {
	s.gcCloser.SignalAndWait()
	if err := s.Pstore.Close(); err != nil {
		glog.Errorf("Error while closing postings store: %v", err)
	}
	if err := s.WALstore.Close(); err != nil {
		glog.Errorf("Error while closing WAL store: %v", err)
	}
}
