package curator

import (
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/samuel/go-zookeeper/zk"
)

const (
	MAX_BACKGROUND_ERRORS = 10
)

var (
	ErrConnectionLoss = errors.New("connection loss")
)

type zookeeperHelper interface {
	GetConnectionString() string
	GetZookeeperConnection() (ZookeeperConnection, error)
}

type zookeeperFactory struct {
	holder *handleHolder
}

func (f *zookeeperFactory) GetConnectionString() string { return "" }
func (f *zookeeperFactory) GetZookeeperConnection() (ZookeeperConnection, error) {
	connectString := f.holder.ensembleProvider.ConnectionString()
	conn, events, err := f.holder.zookeeperDialer.Dial(connectString, f.holder.sessionTimeout, f.holder.canBeReadOnly)

	if err != nil {
		return nil, err
	}

	if events != nil {
		go NewWatchers(f.holder.watcher).Watch(events)
	}

	f.holder.helper = &zookeeperCache{connectString, conn}

	return conn, err
}

type zookeeperCache struct {
	connnectString string
	conn           ZookeeperConnection
}

func (c *zookeeperCache) GetConnectionString() string                          { return c.connnectString }
func (c *zookeeperCache) GetZookeeperConnection() (ZookeeperConnection, error) { return c.conn, nil }

type handleHolder struct {
	zookeeperDialer  ZookeeperDialer
	ensembleProvider EnsembleProvider
	watcher          Watcher
	sessionTimeout   time.Duration
	canBeReadOnly    bool
	helper           zookeeperHelper
}

func (h *handleHolder) getConnectionString() string {
	if h.helper != nil {
		return h.helper.GetConnectionString()
	}

	return ""
}

func (h *handleHolder) hasNewConnectionString() bool {
	if h.helper != nil {
		return h.ensembleProvider.ConnectionString() != h.helper.GetConnectionString()
	}

	return false
}

func (h *handleHolder) getZookeeperConnection() (ZookeeperConnection, error) {
	if h.helper != nil {
		return h.helper.GetZookeeperConnection()
	}

	return nil, nil
}

func (h *handleHolder) closeAndClear() error {
	err := h.internalClose()

	h.helper = nil

	return err
}

func (h *handleHolder) closeAndReset() error {
	if err := h.internalClose(); err != nil {
		return err
	}

	h.helper = &zookeeperFactory{holder: h}

	return nil
}

func (h *handleHolder) internalClose() error {
	if h.helper != nil {
		if conn, err := h.getZookeeperConnection(); err != nil {
			return err
		} else if conn != nil {
			conn.Close()
		}
	}

	return nil
}

type connectionState struct {
	ensembleProvider  EnsembleProvider
	sessionTimeout    time.Duration
	connectionTimeout time.Duration
	tracer            TracerDriver
	parentWatchers    *Watchers
	zooKeeper         *handleHolder
	instanceIndex     int64
	connectionStart   time.Time
	isConnected       AtomicBool
	backgroundErrors  chan error
}

func newConnectionState(zookeeperDialer ZookeeperDialer, ensembleProvider EnsembleProvider, sessionTimeout, connectionTimeout time.Duration,
	parentWatcher Watcher, tracer TracerDriver, canBeReadOnly bool) *connectionState {

	s := &connectionState{
		ensembleProvider:  ensembleProvider,
		sessionTimeout:    sessionTimeout,
		connectionTimeout: connectionTimeout,
		tracer:            tracer,
		parentWatchers:    NewWatchers(),
		connectionStart:   time.Now(),
		backgroundErrors:  make(chan error, MAX_BACKGROUND_ERRORS),
	}

	if zookeeperDialer == nil {
		zookeeperDialer = &DefaultZookeeperDialer{}
	}

	s.zooKeeper = &handleHolder{
		zookeeperDialer:  zookeeperDialer,
		ensembleProvider: ensembleProvider,
		watcher:          s,
		sessionTimeout:   sessionTimeout,
		canBeReadOnly:    canBeReadOnly,
	}

	if parentWatcher != nil {
		s.parentWatchers.Add(parentWatcher)
	}

	return s
}

func (s *connectionState) Connected() bool {
	return s.isConnected.Load()
}

func (s *connectionState) InstanceIndex() int64 {
	return atomic.LoadInt64(&s.instanceIndex)
}

func (s *connectionState) Conn() (ZookeeperConnection, error) {
	if err := s.dequeBackgroundException(); err != nil {
		return nil, err
	}

	if !s.isConnected.Load() {
		if err := s.checkTimeout(); err != nil {
			return nil, err
		}
	}

	return s.zooKeeper.getZookeeperConnection()
}

func (s *connectionState) Start() error {
	if err := s.ensembleProvider.Start(); err != nil {
		return err
	}

	return s.reset()
}

func (s *connectionState) Close() error {
	CloseQuietly(s.ensembleProvider)

	err := s.zooKeeper.closeAndClear()

	s.isConnected.Set(false)

	return err
}

func (s *connectionState) reset() error {
	atomic.AddInt64(&s.instanceIndex, 1)

	s.isConnected.Set(false)

	s.zooKeeper.closeAndReset()

	_, err := s.zooKeeper.getZookeeperConnection() // initiate connection

	return err
}

func (s *connectionState) AddParentWatcher(watcher Watcher) Watcher {
	return s.parentWatchers.Add(watcher)
}

func (s *connectionState) RemoveParentWatcher(watcher Watcher) Watcher {
	return s.parentWatchers.Remove(watcher)
}

func (s *connectionState) checkTimeout() error {
	var minTimeout, maxTimeout time.Duration

	if s.sessionTimeout > s.connectionTimeout {
		minTimeout = s.connectionTimeout
		maxTimeout = s.sessionTimeout
	} else {
		minTimeout = s.sessionTimeout
		maxTimeout = s.connectionTimeout
	}

	elapsed := time.Since(s.connectionStart)

	if elapsed >= minTimeout {
		if s.zooKeeper.hasNewConnectionString() {
			s.handleNewConnectionString()
		} else if elapsed >= maxTimeout {
			log.Printf("Connection attempt unsuccessful after %v (greater than max timeout of %v). Resetting connection and trying again with a new connection.", elapsed, maxTimeout)

			s.tracer.AddCount("session-timed-out", 1)

			return s.reset()
		} else {
			log.Printf("Connection timed out for connection string (%s) and timeout (%v) / elapsed (%v)", s.zooKeeper.getConnectionString(), s.connectionTimeout, elapsed)

			s.tracer.AddCount("connections-timed-out", 1)

			return ErrConnectionLoss
		}
	}

	return nil
}

func (s *connectionState) process(event *zk.Event) {
	log.Printf("connectionState watcher: %v", event)

	for _, watcher := range s.parentWatchers.watchers {
		go func() {
			tracer := newTimeTracer("connection-state-parent-process", s.tracer)

			defer tracer.Commit()

			watcher.process(event)
		}()
	}

	if event.Type == zk.EventSession {
		wasConnected := s.isConnected.Load()

		log.Printf("Connection state changed to %s when connected=%v", event.State, wasConnected)

		if newIsConnected := s.checkState(event.State, event.Err, wasConnected); newIsConnected != wasConnected {
			log.Printf("Connection state changed to %s, connected from %v to %v", event.State, wasConnected, newIsConnected)

			s.isConnected.Set(newIsConnected)
			s.connectionStart = time.Now()
		}
	}
}

func (s *connectionState) checkState(state zk.State, err error, wasConnected bool) bool {
	isConnected := wasConnected
	checkNewConnectionString := true

	switch state {
	case zk.StateHasSession:
		isConnected = true

	case zk.StateExpired:
		isConnected = false
		checkNewConnectionString = false

		s.handleExpiredSession()

	case zk.StateConnecting, zk.StateConnected, zk.StateDisconnected:
	default:
		isConnected = false
	}

	if checkNewConnectionString && s.zooKeeper.hasNewConnectionString() {
		s.handleNewConnectionString()
	}

	return isConnected
}

func (s *connectionState) handleNewConnectionString() {
	log.Print("Connection string changed")

	s.tracer.AddCount("connection-string-changed", 1)

	if err := s.reset(); err != nil {
		s.queueBackgroundException(err)
	}
}

func (s *connectionState) handleExpiredSession() {
	log.Print("Session expired event received")

	s.tracer.AddCount("session-expired", 1)

	if err := s.reset(); err != nil {
		s.queueBackgroundException(err)
	}
}

func (s *connectionState) queueBackgroundException(err error) {
	for {
		select {
		case s.backgroundErrors <- err:
			return
		default:
		}

		if _, ok := <-s.backgroundErrors; !ok {
			return
		} else {
			s.tracer.AddCount("connection-drop-background-error", 1)
		}
	}
}

func (s *connectionState) dequeBackgroundException() error {
	select {
	case err := <-s.backgroundErrors:
		if err != nil {
			s.tracer.AddCount("background-exceptions", 1)

			return err
		}
	default:
	}

	return nil
}

type ConnectionState int32

const (
	UNKNOWN     ConnectionState = iota
	CONNECTED                   // Sent for the first successful connection to the server.
	SUSPENDED                   // There has been a loss of connection. Leaders, locks, etc.
	RECONNECTED                 // A suspended, lost, or read-only connection has been re-established
	LOST                        // The connection is confirmed to be lost. Close any locks, leaders, etc.
	READ_ONLY                   // The connection has gone into read-only mode.
)

func (s ConnectionState) Connected() bool {
	return s == CONNECTED || s == RECONNECTED || s == READ_ONLY
}

type connectionStateManager struct {
	client                 CuratorFramework
	listeners              ConnectionStateListenable
	state                  State
	currentConnectionState ConnectionState
	events                 chan ConnectionState
	QueueSize              int
}

func newConnectionStateManager(client CuratorFramework) *connectionStateManager {
	return &connectionStateManager{
		client:    client,
		listeners: new(connectionStateListenerContainer),
		QueueSize: 25,
	}
}

func (m *connectionStateManager) Start() error {
	if !m.state.Change(LATENT, STARTED) {
		return fmt.Errorf("Cannot be started more than once")
	}

	m.events = make(chan ConnectionState, m.QueueSize)

	go m.processEvents()

	return nil
}

func (m *connectionStateManager) Close() error {
	if !m.state.Change(STARTED, STOPPED) {
		return nil
	}

	close(m.events)

	return nil
}

func (m *connectionStateManager) processEvents() {
	for {
		if newState, ok := <-m.events; !ok {
			return // queue closed
		} else {
			m.listeners.ForEach(func(listener interface{}) {
				listener.(ConnectionStateListener).StateChanged(m.client, newState)
			})
		}
	}
}

func (m *connectionStateManager) postState(state ConnectionState) {
	for {
		select {
		case m.events <- state:
			return
		default:
		}

		select {
		case <-m.events: // "ConnectionStateManager queue full - dropping events to make room"
		default:
		}
	}
}

func (m *connectionStateManager) BlockUntilConnected(maxWaitTime time.Duration) error {
	c := make(chan ConnectionState)

	listener := NewConnectionStateListener(func(client CuratorFramework, newState ConnectionState) {
		if newState.Connected() {
			c <- newState
		}
	})

	m.listeners.AddListener(listener)

	defer m.listeners.RemoveListener(listener)

	if maxWaitTime > 0 {
		timer := time.NewTimer(maxWaitTime)

		select {
		case <-c:
			return nil
		case <-timer.C:
			return errors.New("timeout")
		}
	} else {
		<-c

		return nil
	}
}
