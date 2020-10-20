// Package gocbcore implements methods for low-level communication
// with a Couchbase Server cluster.
package gocbcore

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"golang.org/x/net/http2"
)

// Agent represents the base client handling connections to a Couchbase Server.
// This is used internally by the higher level classes for communicating with the cluster,
// it can also be used to perform more advanced operations with a cluster.
type Agent struct {
	clientID             string
	bucketName           string
	tlsConfig            *dynTLSConfig
	initFn               memdInitFunc
	networkType          string
	useMutationTokens    bool
	useKvErrorMaps       bool
	useEnhancedErrors    bool
	useCompression       bool
	useDurations         bool
	disableDecompression bool

	compressionMinSize  int
	compressionMinRatio float64

	closeNotify       chan struct{}
	cccpLooperDoneSig chan struct{}
	httpLooperDoneSig chan struct{}

	configLock  sync.Mutex
	routingInfo routeDataPtr
	kvErrorMap  kvErrorMapPtr
	numVbuckets int

	tracer           opentracing.Tracer
	noRootTraceSpans bool

	serverFailuresLock sync.Mutex
	serverFailures     map[string]time.Time

	httpCli *http.Client

	confHttpRedialPeriod time.Duration
	confHttpRetryDelay   time.Duration
	confCccpMaxWait      time.Duration
	confCccpPollPeriod   time.Duration

	serverConnectTimeout time.Duration
	serverWaitTimeout    time.Duration
	nmvRetryDelay        time.Duration
	kvPoolSize           int
	maxQueueSize         int

	zombieLock      sync.RWMutex
	zombieOps       []*zombieLogEntry
	useZombieLogger uint32

	dcpPriority   DcpAgentPriority
	useDcpExpiry  bool
	dcpBufferSize int
	dcpQueueSize  int
}

// !!!!UNSURE WHY THESE EXIST!!!!
// ServerConnectTimeout gets the timeout for each server connection, including all authentication steps.
// func (agent *Agent) ServerConnectTimeout() time.Duration {
// 	return agent.kvConnectTimeout
// }
//
// // SetServerConnectTimeout sets the timeout for each server connection.
// func (agent *Agent) SetServerConnectTimeout(timeout time.Duration) {
// 	agent.kvConnectTimeout = timeout
// }

// HTTPClient returns a pre-configured HTTP Client for communicating with
// Couchbase Server.  You must still specify authentication information
// for any dispatched requests.
func (agent *Agent) HTTPClient() *http.Client {
	return agent.http.cli
}

// AuthFunc is invoked by the agent to authenticate a client.
type AuthFunc func(client AuthClient, deadline time.Time) error

// AgentConfig specifies the configuration options for creation of an Agent.
type AgentConfig struct {
	UserString           string
	MemdAddrs            []string
	HttpAddrs            []string
	TlsConfig            *tls.Config
	BucketName           string
	NetworkType          string
	AuthHandler          AuthFunc
	Auth                 AuthProvider
	UseMutationTokens    bool
	UseKvErrorMaps       bool
	UseEnhancedErrors    bool
	UseCompression       bool
	UseDurations         bool
	DisableDecompression bool

	CompressionMinSize  int
	CompressionMinRatio float64

	HttpRedialPeriod time.Duration
	HttpRetryDelay   time.Duration
	CccpMaxWait      time.Duration
	CccpPollPeriod   time.Duration

	ConnectTimeout       time.Duration
	ServerConnectTimeout time.Duration
	NmvRetryDelay        time.Duration
	KvPoolSize           int
	MaxQueueSize         int

	HttpMaxIdleConns        int
	HttpMaxIdleConnsPerHost int
	HttpIdleConnTimeout     time.Duration

	Tracer                 opentracing.Tracer
	NoRootTraceSpans       bool
	UseZombieLogger        bool
	ZombieLoggerInterval   time.Duration
	ZombieLoggerSampleSize int

	DcpAgentPriority DcpAgentPriority
	UseDcpExpiry     bool
	DcpBufferSize    int

	// Username specifies the username to use when connecting.
	// DEPRECATED
	Username string

	// Password specifies the password to use when connecting.
	// DEPRECATED
	Password string
}

// FromConnStr populates the AgentConfig with information from a
// Couchbase Connection String.
// Supported options are:
//   cacertpath (string) - Path to the CA certificate
//   certpath (string) - Path to your authentication certificate
//   keypath (string) - Path to your authentication key
//   config_total_timeout (int) - Maximum period to attempt to connect to cluster in ms.
//   config_node_timeout (int) - Maximum period to attempt to connect to a node in ms.
//   http_redial_period (int) - Maximum period to keep HTTP config connections open in ms.
//   http_retry_delay (int) - Period to wait between retrying nodes for HTTP config in ms.
//   config_poll_floor_interval (int) - Minimum time to wait between fetching configs via CCCP in ms.
//   config_poll_interval (int) - Period to wait between CCCP config polling in ms.
//   kv_pool_size (int) - The number of connections to establish per node.
//   max_queue_size (int) - The maximum size of the operation queues per node.
//   use_kverrmaps (bool) - Whether to enable error maps from the server.
//   use_enhanced_errors (bool) - Whether to enable enhanced error information.
//   fetch_mutation_tokens (bool) - Whether to fetch mutation tokens for operations.
//   compression (bool) - Whether to enable network-wise compression of documents.
//   compression_min_size (int) - The minimal size of the document to consider compression.
//   compression_min_ratio (float64) - The minimal compress ratio (compressed / original) for the document to be sent compressed.
//   server_duration (bool) - Whether to enable fetching server operation durations.
//   http_max_idle_conns (int) - Maximum number of idle http connections in the pool.
//   http_max_idle_conns_per_host (int) - Maximum number of idle http connections in the pool per host.
//   http_idle_conn_timeout (int) - Maximum length of time for an idle connection to stay in the pool in ms.
//   network (string) - The network type to use
func (config *AgentConfig) FromConnStr(connStr string) error {
	baseSpec, err := gocbconnstr.Parse(connStr)
	if err != nil {
		return err
	}

	spec, err := gocbconnstr.Resolve(baseSpec)
	if err != nil {
		return err
	}

	fetchOption := func(name string) (string, bool) {
		optValue := spec.Options[name]
		if len(optValue) == 0 {
			return "", false
		}
		return optValue[len(optValue)-1], true
	}

	// Grab the resolved hostnames into a set of string arrays
	var httpHosts []string
	for _, specHost := range spec.HttpHosts {
		httpHosts = append(httpHosts, fmt.Sprintf("%s:%d", specHost.Host, specHost.Port))
	}

	var memdHosts []string
	for _, specHost := range spec.MemdHosts {
		memdHosts = append(memdHosts, fmt.Sprintf("%s:%d", specHost.Host, specHost.Port))
	}

	// Get bootstrap_on option to determine which, if any, of the bootstrap nodes should be cleared
	switch val, _ := fetchOption("bootstrap_on"); val {
	case "http":
		memdHosts = nil
		if len(httpHosts) == 0 {
			return errors.New("bootstrap_on=http but no HTTP hosts in connection string")
		}
	case "cccp":
		httpHosts = nil
		if len(memdHosts) == 0 {
			return errors.New("bootstrap_on=cccp but no CCCP/Memcached hosts in connection string")
		}
	case "both":
	case "":
		// Do nothing
		break
	default:
		return errors.New("bootstrap_on={http,cccp,both}")
	}
	config.MemdAddrs = memdHosts
	config.HttpAddrs = httpHosts

	var tlsConfig *tls.Config
	if spec.UseSsl {
		var certpath string
		var keypath string
		var cacertpaths []string

		if len(spec.Options["cacertpath"]) > 0 || len(spec.Options["keypath"]) > 0 {
			cacertpaths = spec.Options["cacertpath"]
			certpath, _ = fetchOption("certpath")
			keypath, _ = fetchOption("keypath")
		} else {
			cacertpaths = spec.Options["certpath"]
		}

		tlsConfig = &tls.Config{}

// authFunc wraps AuthFunc to provide a better to the user.
type authFunc func() (completedCh chan BytesAndError, continueCh chan bool, err error)

type authFuncHandler func(client AuthClient, deadline time.Time, mechanism AuthMechanism) authFunc

// CreateAgent creates an agent for performing normal operations.
func CreateAgent(config *AgentConfig) (*Agent, error) {
	initFn := func(client *memdClient, deadline time.Time) error {
		return nil
	}

	return createAgent(config, initFn)
}

func createAgent(config *AgentConfig, initFn memdInitFunc) (*Agent, error) {
	logInfof("SDK Version: gocbcore/%s", goCbCoreVersionStr)
	logInfof("Creating new agent: %+v", config)

	var tlsConfig *dynTLSConfig
	if config.UseTLS {
		tlsConfig = createTLSConfig(config.Auth, config.TLSRootCAProvider)
	}

	httpIdleConnTimeout := 4500 * time.Millisecond
	if config.HTTPIdleConnectionTimeout > 0 {
		httpIdleConnTimeout = config.HTTPIdleConnectionTimeout
	}

	httpCli := createHTTPClient(config.HTTPMaxIdleConns, config.HTTPMaxIdleConnsPerHost,
		httpIdleConnTimeout, tlsConfig)

	tracer := config.Tracer
	if tracer == nil {
		tracer = noopTracer{}
	}
	tracerCmpt := newTracerComponent(tracer, config.BucketName, config.NoRootTraceSpans)

	c := &Agent{
		clientID:   formatCbUID(randomCbUID()),
		bucketName: config.BucketName,
		tlsConfig:  tlsConfig,
		initFn:     initFn,
		tracer:     tracerCmpt,

		defaultRetryStrategy: config.DefaultRetryStrategy,

		errMap: newErrMapManager(config.BucketName),
	}

	circuitBreakerConfig := config.CircuitBreakerConfig
	auth := config.Auth
	userAgent := config.UserAgent
	useMutationTokens := config.UseMutationTokens
	disableDecompression := config.DisableDecompression
	useCompression := config.UseCompression
	useCollections := config.UseCollections
	useJSONHello := !config.DisableJSONHello
	useXErrorHello := !config.DisableXErrors
	useSyncReplicationHello := !config.DisableSyncReplicationHello
	compressionMinSize := 32
	compressionMinRatio := 0.83
	useDurations := config.UseDurations
	useOutOfOrder := config.UseOutOfOrderResponses

	kvConnectTimeout := 7000 * time.Millisecond
	if config.KVConnectTimeout > 0 {
		kvConnectTimeout = config.KVConnectTimeout
	}

	serverWaitTimeout := 5 * time.Second

	kvPoolSize := 1
	if config.KvPoolSize > 0 {
		kvPoolSize = config.KvPoolSize
	}

	maxQueueSize := 2048
	if config.MaxQueueSize > 0 {
		maxQueueSize = config.MaxQueueSize
	}

	confHTTPRetryDelay := 10 * time.Second
	if config.HTTPRetryDelay > 0 {
		confHTTPRetryDelay = config.HTTPRetryDelay
	}

	if valStr, ok := fetchOption("network"); ok {
		config.NetworkType = valStr
	}

	confCccpMaxWait := 3 * time.Second
	if config.CccpMaxWait > 0 {
		confCccpMaxWait = config.CccpMaxWait
	}

	confCccpPollPeriod := 2500 * time.Millisecond
	if config.CccpPollPeriod > 0 {
		confCccpPollPeriod = config.CccpPollPeriod
	}

	if valStr, ok := fetchOption("dcp_buffer_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("dcp buffer size option must be a number")
		}
<<<<<<< HEAD
		config.DcpBufferSize = int(val)
=======
		config.ZombieLoggerSampleSize = int(val)
	}

	if valStr, ok := fetchOption("network"); ok {
		config.NetworkType = valStr
	}

	if valStr, ok := fetchOption("dcp_priority"); ok {
		var priority DcpAgentPriority
		switch valStr {
		case "":
			priority = DcpAgentPriorityLow
		case "low":
			priority = DcpAgentPriorityLow
		case "medium":
			priority = DcpAgentPriorityMed
		case "high":
			priority = DcpAgentPriorityHigh
		default:
			return fmt.Errorf("dcp_priority must be one of low, medium or high")
		}
		config.DcpAgentPriority = priority
	}

	if valStr, ok := fetchOption("enable_expiry_opcode"); ok {
		val, err := strconv.ParseBool(valStr)
		if err != nil {
			return fmt.Errorf("enable_expiry_opcode option must be a boolean")
		}
		config.UseDcpExpiry = val
>>>>>>> abe1401414f3433231fa0fc7932052b1b3b8b5b8
	}

	if valStr, ok := fetchOption("dcp_buffer_size"); ok {
		val, err := strconv.ParseInt(valStr, 10, 64)
		if err != nil {
			return fmt.Errorf("dcp buffer size option must be a number")
		}
		config.DcpBufferSize = int(val)
	}

	return nil
}

func makeDefaultAuthHandler(authProvider AuthProvider, bucketName string) AuthFunc {
	return func(client AuthClient, deadline time.Time) error {
		creds, err := getKvAuthCreds(authProvider, client.Address())
		if err != nil {
			return err
		}

		if creds.Username != "" || creds.Password != "" {
			if err := SaslAuthPlain(creds.Username, creds.Password, client, deadline); err != nil {
				return err
			}
		}

		if client.SupportsFeature(FeatureSelectBucket) {
			if err := client.ExecSelectBucket([]byte(bucketName), deadline); err != nil {
				return err
			}
		}

		return nil
	}
	if config.CompressionMinRatio > 0 {
		compressionMinRatio = config.CompressionMinRatio
		if compressionMinRatio >= 1.0 {
			compressionMinRatio = 1.0
		}
	}
	if c.defaultRetryStrategy == nil {
		c.defaultRetryStrategy = newFailFastRetryStrategy()
	}

	authMechanisms := config.AuthMechanisms
	if len(authMechanisms) == 0 {
		if config.UseTLS {
			authMechanisms = []AuthMechanism{PlainAuthMechanism}
		} else {
			// No user specified auth mechanisms so set our defaults.
			authMechanisms = []AuthMechanism{
				ScramSha512AuthMechanism,
				ScramSha256AuthMechanism,
				ScramSha1AuthMechanism}
		}
	} else if !config.UseTLS {
		// The user has specified their own mechanisms and not using TLS so we check if they've set PLAIN.
		for _, mech := range authMechanisms {
			if mech == PlainAuthMechanism {
				logWarnf("PLAIN sends credentials in plaintext, this will cause credential leakage on the network")
			}
		}
		if err := client.ExecEnableDcpClientEnd(deadline); err != nil {
			return err
		}
<<<<<<< HEAD
=======
		if agent.useDcpExpiry {
			if err := client.ExecDcpControl("enable_expiry_opcode", "true", deadline); err != nil {
				return err
			}
		}
		if err := client.ExecEnableDcpClientEnd(deadline); err != nil {
			return err
		}
>>>>>>> abe1401414f3433231fa0fc7932052b1b3b8b5b8
		return client.ExecEnableDcpBufferAck(agent.dcpBufferSize, deadline)
	}

	return createAgent(config, initFn)
}

func createAgent(config *AgentConfig, initFn memdInitFunc) (*Agent, error) {
	// TODO(brett19): Put all configurable options in the AgentConfig

	logDebugf("SDK Version: gocb/%s", goCbCoreVersionStr)
	logDebugf("Creating new agent: %+v", config)

	httpTransport := &http.Transport{
		TLSClientConfig: config.TlsConfig,
		Dial: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).Dial,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        config.HttpMaxIdleConns,
		MaxIdleConnsPerHost: config.HttpMaxIdleConnsPerHost,
		IdleConnTimeout:     config.HttpIdleConnTimeout,
	}
	err := http2.ConfigureTransport(httpTransport)
	if err != nil {
		logDebugf("failed to configure http2: %s", err)
	}

	tracer := config.Tracer
	if tracer == nil {
		tracer = opentracing.NoopTracer{}
	}

	c := &Agent{
		clientId:    formatCbUid(randomCbUid()),
		userString:  config.UserString,
		bucket:      config.BucketName,
		auth:        config.Auth,
		tlsConfig:   config.TlsConfig,
		initFn:      initFn,
		networkType: config.NetworkType,
		httpCli: &http.Client{
			Transport: httpTransport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// All that we're doing here is setting auth on any redirects.
				// For that reason we can just pull it off the oldest (first) request.
				if len(via) >= 10 {
					// Just duplicate the default behaviour for maximum redirects.
					return errors.New("stopped after 10 redirects")
				}

				oldest := via[0]
				auth := oldest.Header.Get("Authorization")
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}

				return nil
			},
		},
		closeNotify:          make(chan struct{}),
		tracer:               tracer,
		useMutationTokens:    config.UseMutationTokens,
		useKvErrorMaps:       config.UseKvErrorMaps,
		useEnhancedErrors:    config.UseEnhancedErrors,
		useCompression:       config.UseCompression,
		compressionMinSize:   32,
		compressionMinRatio:  0.83,
		useDurations:         config.UseDurations,
		noRootTraceSpans:     config.NoRootTraceSpans,
		serverFailures:       make(map[string]time.Time),
		serverConnectTimeout: 7000 * time.Millisecond,
		serverWaitTimeout:    5 * time.Second,
		nmvRetryDelay:        100 * time.Millisecond,
		kvPoolSize:           1,
		maxQueueSize:         2048,
		confHttpRetryDelay:   10 * time.Second,
		confHttpRedialPeriod: 10 * time.Second,
		confCccpMaxWait:      3 * time.Second,
		confCccpPollPeriod:   2500 * time.Millisecond,
		dcpPriority:          config.DcpAgentPriority,
		disableDecompression: config.DisableDecompression,
		useDcpExpiry:         config.UseDcpExpiry,
		dcpBufferSize:        8 * 1024 * 1024,
	}

	connectTimeout := 60000 * time.Millisecond
	if config.ConnectTimeout > 0 {
		connectTimeout = config.ConnectTimeout
	}

	var httpEpList []string
	for _, hostPort := range config.HTTPAddrs {
		if !c.IsSecure() {
			httpEpList = append(httpEpList, fmt.Sprintf("http://%s", hostPort))
		} else {
			httpEpList = append(httpEpList, fmt.Sprintf("https://%s", hostPort))
		}
	}
	if config.DcpBufferSize > 0 {
		c.dcpBufferSize = config.DcpBufferSize
<<<<<<< HEAD
=======
	}
	c.dcpQueueSize = (c.dcpBufferSize + 23) / 24

	deadline := time.Now().Add(connectTimeout)
	if err := c.connect(config.MemdAddrs, config.HttpAddrs, deadline); err != nil {
		return nil, err
>>>>>>> abe1401414f3433231fa0fc7932052b1b3b8b5b8
	}
	c.dcpQueueSize = (c.dcpBufferSize + 23) / 24

	if config.UseZombieLogger {
		// We setup the zombie logger after connecting so that we don't end up leaking the logging goroutine.
		// We also don't enable the zombie logger on the agent until here so that the operations performed
		// when connecting don't trigger a zombie log to occur when the logger isn't yet setup.
		atomic.StoreUint32(&c.useZombieLogger, 1)
		zombieLoggerInterval := 10 * time.Second
		zombieLoggerSampleSize := 10
		if config.ZombieLoggerInterval > 0 {
			zombieLoggerInterval = config.ZombieLoggerInterval
		}
		if config.ZombieLoggerSampleSize > 0 {
			zombieLoggerSampleSize = config.ZombieLoggerSampleSize
		}
		// zombieOps must have a static capacity for its lifetime, the capacity should
		// never be altered so that it is consistent across the zombieLogger and
		// recordZombieResponse.
		c.zombieOps = make([]*zombieLogEntry, 0, zombieLoggerSampleSize)
		go c.zombieLogger(zombieLoggerInterval, zombieLoggerSampleSize)
	}

	return c, nil
}

func (agent *Agent) connect(memdAddrs, httpAddrs []string, deadline time.Time) error {
	logDebugf("Attempting to connect...")

	for _, thisHostPort := range memdAddrs {
		logDebugf("Trying server at %s", thisHostPort)

		srvDeadlineTm := time.Now().Add(agent.serverConnectTimeout)
		if srvDeadlineTm.After(deadline) {
			srvDeadlineTm = deadline
		}

		logDebugf("Trying to connect")
		client, err := agent.dialMemdClient(thisHostPort)
		if isAccessError(err) {
			return err
		} else if err != nil {
			logDebugf("Connecting failed! %v", err)
			continue
		}

		disconnectClient := func() {
			err := client.Close()
			if err != nil {
				logErrorf("Failed to shut down client connection (%s)", err)
			}
		}

		syncCli := syncClient{
			client: client,
		}

		logDebugf("Attempting to request CCCP configuration")
		cccpBytes, err := syncCli.ExecCccpRequest(srvDeadlineTm)
		if err != nil {
			logDebugf("Failed to retrieve CCCP config. %v", err)
			disconnectClient()
			continue
		}

		hostName, err := hostFromHostPort(thisHostPort)
		if err != nil {
			logErrorf("Failed to parse CCCP source address. %v", err)
			disconnectClient()
			continue
		}

		bk, err := parseConfig(cccpBytes, hostName)
		if err != nil {
			logDebugf("Failed to parse CCCP configuration. %v", err)
			disconnectClient()
			continue
		}

		if !bk.supportsCccp() {
			logDebugf("Bucket does not support CCCP")
			disconnectClient()
			break
		}

		routeCfg := agent.buildFirstRouteConfig(bk, thisHostPort)
		logDebugf("Using network type %s for connections", agent.networkType)
		if !routeCfg.IsValid() {
			logDebugf("Configuration was deemed invalid %+v", routeCfg)
			disconnectClient()
			continue
		}

		logDebugf("Successfully connected")

		c.zombieLogger = newZombieLoggerComponent(zombieLoggerInterval, zombieLoggerSampleSize)
		go c.zombieLogger.Start()
	}

	c.cfgManager = newConfigManager(
		configManagerProperties{
			NetworkType:  config.NetworkType,
			UseSSL:       config.UseTLS,
			SrcMemdAddrs: config.MemdAddrs,
			SrcHTTPAddrs: httpEpList,
		},
	)

	dialer := newMemdClientDialerComponent(
		memdClientDialerProps{
			ServerWaitTimeout:    serverWaitTimeout,
			KVConnectTimeout:     kvConnectTimeout,
			ClientID:             c.clientID,
			TLSConfig:            c.tlsConfig,
			CompressionMinSize:   compressionMinSize,
			CompressionMinRatio:  compressionMinRatio,
			DisableDecompression: disableDecompression,
		},
		bootstrapProps{
			HelloProps: helloProps{
				CollectionsEnabled:     useCollections,
				MutationTokensEnabled:  useMutationTokens,
				CompressionEnabled:     useCompression,
				DurationsEnabled:       useDurations,
				OutOfOrderEnabled:      useOutOfOrder,
				JSONFeatureEnabled:     useJSONHello,
				XErrorFeatureEnabled:   useXErrorHello,
				SyncReplicationEnabled: useSyncReplicationHello,
			},
			Bucket:         c.bucketName,
			UserAgent:      userAgent,
			AuthMechanisms: authMechanisms,
			AuthHandler:    authHandler,
			ErrMapManager:  c.errMap,
		},
		circuitBreakerConfig,
		c.zombieLogger,
		c.tracer,
		initFn,
	)
	c.kvMux = newKVMux(
		kvMuxProps{
			QueueSize:          maxQueueSize,
			PoolSize:           kvPoolSize,
			CollectionsEnabled: useCollections,
		},
		c.cfgManager,
		c.errMap,
		c.tracer,
		dialer,
	)
	c.collections = newCollectionIDManager(
		collectionIDProps{
			MaxQueueSize:         config.MaxQueueSize,
			DefaultRetryStrategy: c.defaultRetryStrategy,
		},
		c.kvMux,
		c.tracer,
		c.cfgManager,
	)
	c.httpMux = newHTTPMux(circuitBreakerConfig, c.cfgManager)
	c.http = newHTTPComponent(
		httpComponentProps{
			UserAgent:            userAgent,
			DefaultRetryStrategy: c.defaultRetryStrategy,
		},
		httpCli,
		c.httpMux,
		auth,
		c.tracer,
	)

	if len(config.MemdAddrs) == 0 && config.BucketName == "" {
		// The http poller can't run without a bucket. We don't trigger an error for this case
		// because AgentGroup users who use memcached buckets on non-default ports will end up here.
		logDebugf("No bucket name specified and only http addresses specified, not running config poller")
	} else {
		c.pollerController = newPollerController(
			newCCCPConfigController(
				cccpPollerProperties{
					confCccpMaxWait:    confCccpMaxWait,
					confCccpPollPeriod: confCccpPollPeriod,
				},
				c.kvMux,
				c.cfgManager,
			),
			newHTTPConfigController(
				c.bucketName,
				httpPollerProperties{
					httpComponent:        c.http,
					confHTTPRetryDelay:   confHTTPRetryDelay,
					confHTTPRedialPeriod: confHTTPRedialPeriod,
				},
				c.httpMux,
				c.cfgManager,
			),
			c.cfgManager,
		)
	}

	c.observe = newObserveComponent(c.collections, c.defaultRetryStrategy, c.tracer, c.kvMux)
	c.crud = newCRUDComponent(c.collections, c.defaultRetryStrategy, c.tracer, c.errMap, c.kvMux)
	c.stats = newStatsComponent(c.kvMux, c.defaultRetryStrategy, c.tracer)
	c.n1ql = newN1QLQueryComponent(c.http, c.cfgManager, c.tracer)
	c.analytics = newAnalyticsQueryComponent(c.http, c.tracer)
	c.search = newSearchQueryComponent(c.http, c.tracer)
	c.views = newViewQueryComponent(c.http, c.tracer)
	c.diagnostics = newDiagnosticsComponent(c.kvMux, c.httpMux, c.http, c.bucketName, c.defaultRetryStrategy, c.pollerController)

	// Kick everything off.
	cfg := &routeConfig{
		kvServerList: config.MemdAddrs,
		mgmtEpList:   httpEpList,
		revID:        -1,
	}

	c.httpMux.OnNewRouteConfig(cfg)
	c.kvMux.OnNewRouteConfig(cfg)

	if c.pollerController != nil {
		go c.pollerController.Start()
	}

	return c, nil
}

func createTLSConfig(auth AuthProvider, caProvider func() *x509.CertPool) *dynTLSConfig {
	return &dynTLSConfig{
		BaseConfig: &tls.Config{
			GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
				cert, err := auth.Certificate(AuthCertRequest{})
				if err != nil {
					return nil, err
				}

				if cert == nil {
					return &tls.Certificate{}, nil
				}

				return cert, nil
			},
			MinVersion: tls.VersionTLS12,
		},
		Provider: caProvider,
	}
}

func createHTTPClient(maxIdleConns, maxIdleConnsPerHost int, idleTimeout time.Duration, tlsConfig *dynTLSConfig) *http.Client {
	httpDialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	// We set up the transport to point at the BaseConfig from the dynamic TLS system.
	// We also set ForceAttemptHTTP2, which will update the base-config to support HTTP2
	// automatically, so that all configs from it will look for that.

	var httpTLSConfig *dynTLSConfig
	var httpBaseTLSConfig *tls.Config
	if tlsConfig != nil {
		httpTLSConfig = tlsConfig.Clone()
		httpBaseTLSConfig = httpTLSConfig.BaseConfig
	}

	httpTransport := &http.Transport{
		TLSClientConfig:   httpBaseTLSConfig,
		ForceAttemptHTTP2: true,

		Dial: func(network, addr string) (net.Conn, error) {
			return httpDialer.Dial(network, addr)
		},
		DialTLS: func(network, addr string) (net.Conn, error) {
			tcpConn, err := httpDialer.Dial(network, addr)
			if err != nil {
				return nil, err
			}

			if httpTLSConfig == nil {
				return nil, errors.New("TLS was not configured on this Agent")
			}
			srvTLSConfig, err := httpTLSConfig.MakeForAddr(addr)
			if err != nil {
				return nil, err
			}

			tlsConn := tls.Client(tcpConn, srvTLSConfig)
			return tlsConn, nil
		},
		MaxIdleConns:        maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout:     idleTimeout,
	}

	httpCli := &http.Client{
		Transport: httpTransport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// All that we're doing here is setting auth on any redirects.
			// For that reason we can just pull it off the oldest (first) request.
			if len(via) >= 10 {
				// Just duplicate the default behaviour for maximum redirects.
				return errors.New("stopped after 10 redirects")
			}

			oldest := via[0]
			auth := oldest.Header.Get("Authorization")
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}

			return nil
		},
	}
	return httpCli
}

func buildAuthHandler(auth AuthProvider) authFuncHandler {
	return func(client AuthClient, deadline time.Time, mechanism AuthMechanism) authFunc {
		creds, err := getKvAuthCreds(auth, client.Address())
		if err != nil {
			return nil
		}

		if creds.Username != "" || creds.Password != "" {
			return func() (chan BytesAndError, chan bool, error) {
				continueCh := make(chan bool, 1)
				completedCh := make(chan BytesAndError, 1)
				hasContinued := int32(0)
				callErr := saslMethod(mechanism, creds.Username, creds.Password, client, deadline, func() {
					// hasContinued should never be 1 here but let's guard against it.
					if atomic.CompareAndSwapInt32(&hasContinued, 0, 1) {
						continueCh <- true
					}
				}, func(err error) {
					if atomic.CompareAndSwapInt32(&hasContinued, 0, 1) {
						sendContinue := true
						if err != nil {
							sendContinue = false
						}
						continueCh <- sendContinue
					}
					completedCh <- BytesAndError{Err: err}
				})
				if callErr != nil {
					return nil, nil, err
				}
				return completedCh, continueCh, nil
			}
		}

		return nil
	}
}

// Close shuts down the agent, disconnecting from all servers and failing
// any outstanding operations with ErrShutdown.
func (agent *Agent) Close() error {
	routeCloseErr := agent.kvMux.Close()

	poller := agent.pollerController
	if poller != nil {
		poller.Stop()
	}

	if agent.zombieLogger != nil {
		agent.zombieLogger.Stop()
	}

	if poller != nil {
		// Wait for our external looper goroutines to finish, note that if the
		// specific looper wasn't used, it will be a nil value otherwise it
		// will be an open channel till its closed to signal completion.
		pollerCh := poller.Done()
		if pollerCh != nil {
			<-pollerCh
		}
	}

	// Close the transports so that they don't hold open goroutines.
	agent.http.Close()

	return routeCloseErr
}

// ClientID returns the unique id for this agent
func (agent *Agent) ClientID() string {
	return agent.clientID
}

// CapiEps returns all the available endpoints for performing
// map-reduce queries.
func (agent *Agent) CapiEps() []string {
	return agent.httpMux.CapiEps()
}

// MgmtEps returns all the available endpoints for performing
// management queries.
func (agent *Agent) MgmtEps() []string {
	return agent.httpMux.MgmtEps()
}

// N1qlEps returns all the available endpoints for performing
// N1QL queries.
func (agent *Agent) N1qlEps() []string {
	return agent.httpMux.N1qlEps()
}

// FtsEps returns all the available endpoints for performing
// FTS queries.
func (agent *Agent) FtsEps() []string {
	return agent.httpMux.FtsEps()
}

// CbasEps returns all the available endpoints for performing
// CBAS queries.
func (agent *Agent) CbasEps() []string {
	return agent.httpMux.CbasEps()
}

// HasCollectionsSupport verifies whether or not collections are available on the agent.
func (agent *Agent) HasCollectionsSupport() bool {
	return agent.kvMux.SupportsCollections()
}

// IsSecure returns whether this client is connected via SSL.
func (agent *Agent) IsSecure() bool {
	return agent.tlsConfig != nil
}

// UsingGCCCP returns whether or not the Agent is currently using GCCCP polling.
func (agent *Agent) UsingGCCCP() bool {
	return agent.kvMux.SupportsGCCCP()
}

// HasSeenConfig returns whether or not the Agent has seen a valid cluster config. This does not mean that the agent
// currently has active connections.
// Volatile: This API is subject to change at any time.
func (agent *Agent) HasSeenConfig() (bool, error) {
	seen, err := agent.kvMux.ConfigRev()
	if err != nil {
		return false, err
	}

	return seen > -1, nil
}

// WaitUntilReady returns whether or not the Agent has seen a valid cluster config.
func (agent *Agent) WaitUntilReady(deadline time.Time, opts WaitUntilReadyOptions, cb WaitUntilReadyCallback) (PendingOp, error) {
	return agent.diagnostics.WaitUntilReady(deadline, opts, cb)
}

// ConfigSnapshot returns a snapshot of the underlying configuration currently in use.
func (agent *Agent) ConfigSnapshot() (*ConfigSnapshot, error) {
	return agent.kvMux.ConfigSnapshot()
}

// BucketName returns the name of the bucket that the agent is using, if any.
// Uncommitted: This API may change in the future.
func (agent *Agent) BucketName() string {
	return agent.bucketName
}
