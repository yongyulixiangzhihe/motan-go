package endpoint

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	motan "github.com/weibocom/motan-go/core"
	"github.com/weibocom/motan-go/log"
	mpro "github.com/weibocom/motan-go/protocol"
)

var (
	defaultChannelPoolSize     = 3
	defaultRequestTimeout      = 1000 * time.Millisecond
	defaultConnectTimeout      = 1000 * time.Millisecond
	defaultKeepaliveInterval   = 10 * time.Second
	defaultErrorCountThreshold = 10
	ErrChannelShutdown         = fmt.Errorf("The channel has been shutdown.")
	ErrRequestTimeout          = fmt.Errorf("Timeout err: request timeout.")

	idOffset            uint64 // id generator offset
	defaultAsyncResonse = &motan.MotanResponse{Attachment: make(map[string]string, 0), RpcContext: &motan.RpcContext{AsyncCall: true}}
)

type MotanEndpoint struct {
	url        *motan.Url
	channels   *ChannelPool
	destroyCh  chan struct{}
	available  bool
	mux        sync.RWMutex
	errorCount uint32
	proxy      bool

	// for heartbeat requestid
	keepaliveID   uint64
	serialization motan.Serialization
}

func (m *MotanEndpoint) setAvailable(available bool) {
	m.mux.Lock()
	m.available = available
	m.mux.Unlock()
}

func (m *MotanEndpoint) SetSerialization(s motan.Serialization) {
	m.serialization = s
}

func (m *MotanEndpoint) SetProxy(proxy bool) {
	m.proxy = proxy
}

func (m *MotanEndpoint) Initialize() {
	m.destroyCh = make(chan struct{}, 1)
	connectTimeout := m.url.GetTimeDuration("connectTimeout", time.Millisecond, defaultConnectTimeout)

	factory := func() (net.Conn, error) {
		return net.DialTimeout("tcp", m.url.Host+":"+strconv.Itoa((int)(m.url.Port)), connectTimeout)
	}
	channels, err := NewChannelPool(defaultChannelPoolSize, factory, nil)
	if err != nil {
		vlog.Errorln("Channel pool init failed. ", err)
		// retry connect
		go func() {
			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					channels, err := NewChannelPool(defaultChannelPoolSize, factory, nil)
					if err == nil {
						m.channels = channels
						m.setAvailable(true)
						return
					}
				case <-m.destroyCh:
					return
				}
			}
		}()
	} else {
		m.channels = channels
		m.setAvailable(true)
	}
}

func (m *MotanEndpoint) Destroy() {
	m.setAvailable(false)
	m.destroyCh <- struct{}{}
	if m.channels != nil {
		vlog.Infof("motan2 endpoint %s will destroyed", m.url.GetAddressStr())
		m.channels.Close()
	}
}

func (m *MotanEndpoint) Call(request motan.Request) motan.Response {
	rc := request.GetRpcContext(true)
	rc.Proxy = m.proxy
	if m.channels == nil {
		vlog.Errorln("motanEndpoint error: channels is null")
		m.recordErrAndKeepalive()
		return m.defaultErrMotanResponse(request, "motanEndpoint error: channels is null")
	}
	startTime := time.Now().UnixNano()
	if rc != nil && rc.AsyncCall {
		rc.Result.StartTime = startTime
	}
	// get a channel
	channel, err := m.channels.Get()
	if err != nil {
		vlog.Errorln("motanEndpoint error: can not get a channel.", err)
		m.recordErrAndKeepalive()
		return m.defaultErrMotanResponse(request, "can not get a channel")
	}
	// get request timeout
	deadline := m.url.GetTimeDuration("requestTimeout", time.Millisecond, defaultRequestTimeout)

	// do call
	group := GetRequestGroup(request)
	if group != m.url.Group && m.url.Group != "" {
		request.SetAttachment(mpro.M_group, m.url.Group)
	}

	var msg *mpro.Message
	msg, err = mpro.ConvertToReqMessage(request, m.serialization)

	if err != nil {
		vlog.Errorf("convert motan request fail! %s, err:%s\n", motan.GetReqInfo(request), err.Error())
		return motan.BuildExceptionResponse(request.GetRequestId(), &motan.Exception{ErrCode: 500, ErrMsg: "convert motan request fail!", ErrType: motan.ServiceException})
	}
	recvMsg, err := channel.Call(msg, deadline, rc)
	if err != nil {
		vlog.Errorln("motanEndpoint error: ", err)
		m.recordErrAndKeepalive()
		return m.defaultErrMotanResponse(request, "channel call error:"+err.Error())
	} else {
		// reset errorCount
		m.resetErr()
	}
	if rc != nil && rc.AsyncCall {
		return defaultAsyncResonse
	} else {
		recvMsg.Header.SetProxy(m.proxy)
		response, err := mpro.ConvertToResponse(recvMsg, m.serialization)
		if err != nil {
			vlog.Errorf("convert to response fail.%s, err:%s\n", motan.GetReqInfo(request), err.Error())
			return motan.BuildExceptionResponse(request.GetRequestId(), &motan.Exception{ErrCode: 500, ErrMsg: "convert response fail!" + err.Error(), ErrType: motan.ServiceException})

		}
		response.ProcessDeserializable(rc.Reply)
		response.SetProcessTime(int64((time.Now().UnixNano() - startTime) / 1000000))
		return response
	}

}

func (m *MotanEndpoint) recordErrAndKeepalive() {
	errCount := atomic.AddUint32(&m.errorCount, 1)
	if errCount == uint32(defaultErrorCountThreshold) {
		m.setAvailable(false)
		vlog.Infoln("Referer disable:" + m.url.GetIdentity())
		go m.keepalive()
	}
}

func (m *MotanEndpoint) resetErr() {
	atomic.StoreUint32(&m.errorCount, 0)
}

func (m *MotanEndpoint) keepalive() {
	ticker := time.NewTicker(defaultKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.keepaliveID++
			vlog.Infof("[keepalive] send heartbeat... requestId=%d ", m.keepaliveID)
			if channel, err := m.channels.Get(); err != nil {
				vlog.Infof("failed. %v\n", err)
			} else {
				_, error := channel.Call(mpro.BuildHeartbeat(m.keepaliveID, mpro.Req), defaultRequestTimeout, nil)
				if error == nil {
					m.setAvailable(true)
					vlog.Infof("heartbeat sucess.\n")
					return
				} else {
					vlog.Infof("heartbeat failed. %v\n", err)
				}
			}
		case <-m.destroyCh:
			return
		}
	}
}

func (m *MotanEndpoint) defaultErrMotanResponse(request motan.Request, errMsg string) motan.Response {
	response := &motan.MotanResponse{
		RequestId:  request.GetRequestId(),
		Attachment: make(map[string]string),
		Exception: &motan.Exception{
			ErrCode: 400,
			ErrMsg:  errMsg,
			ErrType: motan.ServiceException,
		},
	}
	return response
}

func (m *MotanEndpoint) GetName() string {
	return "motanEndpoint"
}

func (m *MotanEndpoint) GetUrl() *motan.Url {
	return m.url
}

func (m *MotanEndpoint) SetUrl(url *motan.Url) {
	m.url = url
}

func (m *MotanEndpoint) IsAvailable() bool {
	m.mux.RLock()
	defer m.mux.RUnlock()
	return m.available
}

// config
type Config struct {
	RequestTimeout time.Duration
}

func DefaultConfig() *Config {
	return &Config{
		RequestTimeout: defaultRequestTimeout,
	}
}

func VerifyConfig(config *Config) error {
	if config.RequestTimeout <= 0 {
		return fmt.Errorf("RequestTimeout interval must be positive")
	}
	return nil
}

type Channel struct {
	// config
	config *Config

	// connection
	conn    io.ReadWriteCloser
	bufRead *bufio.Reader

	// send
	sendCh chan sendReady

	// stream
	streams    map[uint64]*Stream
	streamLock sync.Mutex
	// heartbeat
	heartbeats    map[uint64]*Stream
	heartbeatLock sync.Mutex

	// shutdown
	shutdown     bool
	shutdownErr  error
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

type Stream struct {
	channel *Channel
	sendMsg *mpro.Message
	// recv msg
	recvMsg      *mpro.Message
	recvLock     sync.Mutex
	recvNotifyCh chan struct{}
	// timeout
	deadline        time.Time
	originRequestId uint64
	localRequestId  uint64

	rc      *motan.RpcContext
	isClose bool
}

func (s *Stream) Send() error {
	timer := time.NewTimer(s.deadline.Sub(time.Now()))
	defer timer.Stop()

	s.sendMsg.Header.RequestId = s.localRequestId
	buf := s.sendMsg.Encode()
	s.sendMsg.Header.RequestId = s.originRequestId

	ready := sendReady{data: buf.Bytes()}
	select {
	case s.channel.sendCh <- ready:
		return nil
	case <-timer.C:
		return ErrRequestTimeout
	case <-s.channel.shutdownCh:
		return ErrChannelShutdown
	}
}

//sync recv
func (s *Stream) Recv() (*mpro.Message, error) {
	defer func() {
		s.Close()
	}()
	timer := time.NewTimer(s.deadline.Sub(time.Now()))
	defer timer.Stop()
	select {
	case <-s.recvNotifyCh:
		s.recvLock.Lock()
		msg := s.recvMsg
		s.recvLock.Unlock()
		if msg == nil {
			return nil, errors.New("recv err: recvMsg is nil")
		}
		msg.Header.RequestId = s.originRequestId
		return msg, nil
	case <-timer.C:
		return nil, ErrRequestTimeout
	case <-s.channel.shutdownCh:
		return nil, ErrChannelShutdown
	}
}

func (s *Stream) notify(msg *mpro.Message) {
	defer func() {
		s.Close()
	}()
	if s.rc != nil && s.rc.AsyncCall {
		msg.Header.SetProxy(s.rc.Proxy)
		result := s.rc.Result
		serialization := s.rc.ExtFactory.GetSerialization("", msg.Header.GetSerialize())
		response, err := mpro.ConvertToResponse(msg, serialization)
		if err != nil {
			vlog.Errorf("convert to response fail. requestid:%d, err:%s\n", msg.Header.RequestId, err.Error())
			result.Error = err
			result.Done <- result
			return
		}
		response.ProcessDeserializable(result.Reply)
		response.SetProcessTime(int64((time.Now().UnixNano() - result.StartTime) / 1000000))
		result.Done <- result
		return
	} else {
		s.recvLock.Lock()
		s.recvMsg = msg
		s.recvLock.Unlock()
		s.recvNotifyCh <- struct{}{}
	}
}

func (s *Stream) SetDeadline(deadline time.Duration) {
	s.deadline = time.Now().Add(deadline)
}

func (c *Channel) NewStream(msg *mpro.Message, rc *motan.RpcContext) (*Stream, error) {
	if msg == nil || msg.Header == nil {
		return nil, errors.New("msg is invalid.")
	}
	if c.IsClosed() {
		return nil, ErrChannelShutdown
	}
	s := &Stream{
		channel:         c,
		sendMsg:         msg,
		recvNotifyCh:    make(chan struct{}, 1),
		deadline:        time.Now().Add(1 * time.Second),
		originRequestId: msg.Header.RequestId,
		rc:              rc,
	}
	if s.originRequestId == 0 {
		s.localRequestId = GenerateRequestId()
	} else {
		s.localRequestId = s.originRequestId
	}
	if msg.Header.IsHeartbeat() {
		c.heartbeatLock.Lock()
		c.heartbeats[s.localRequestId] = s
		c.heartbeatLock.Unlock()
	} else {
		c.streamLock.Lock()
		c.streams[s.localRequestId] = s
		c.streamLock.Unlock()
	}
	return s, nil
}

func (s *Stream) Close() {
	if !s.isClose {
		s.channel.streamLock.Lock()
		delete(s.channel.streams, s.sendMsg.Header.RequestId)
		s.channel.streamLock.Unlock()
		s.isClose = true
	}
}

type sendReady struct {
	data []byte
}

func (c *Channel) Call(msg *mpro.Message, deadline time.Duration, rc *motan.RpcContext) (*mpro.Message, error) {
	stream, err := c.NewStream(msg, rc)
	if err != nil {
		return nil, err
	}
	stream.SetDeadline(deadline)
	if err := stream.Send(); err != nil {
		return nil, err
	} else {
		if rc != nil && rc.AsyncCall {
			return nil, nil
		} else {
			return stream.Recv()
		}

	}
}

func (c *Channel) IsClosed() bool {
	select {
	case <-c.shutdownCh:
		return true
	default:
		return false
	}
}

func (c *Channel) recv() {
	if err := c.recvLoop(); err != nil {
		c.closeOnErr(fmt.Errorf("%+v", err))
	}
}

func (c *Channel) recvLoop() error {
	for {
		res, err := mpro.DecodeFromReader(c.bufRead)
		if err != nil {
			return err
		}
		var handleErr error
		if res.Header.IsHeartbeat() {
			handleErr = c.handleHeartbeat(res)
		} else {
			handleErr = c.handleMessage(res)
		}
		if handleErr != nil {
			return handleErr
		}
	}
}

func (c *Channel) send() {
	for {
		select {
		case ready := <-c.sendCh:
			if ready.data != nil {
				sent := 0
				for sent < len(ready.data) {
					n, err := c.conn.Write(ready.data[sent:])
					if err != nil {
						vlog.Errorf("Failed to write header: %v", err)
						c.closeOnErr(err)
						return
					}
					sent += n
				}
			}
		case <-c.shutdownCh:
			return
		}
	}
}

func (c *Channel) handleHeartbeat(msg *mpro.Message) error {
	c.heartbeatLock.Lock()
	stream := c.heartbeats[msg.Header.RequestId]
	c.heartbeatLock.Unlock()
	if stream == nil {
		vlog.Warningln("handle heartbeat message, missing stream: ", msg.Header.RequestId)
	} else {
		stream.notify(msg)
	}
	return nil
}

func (c *Channel) handleMessage(msg *mpro.Message) error {
	c.streamLock.Lock()
	stream := c.streams[msg.Header.RequestId]
	c.streamLock.Unlock()
	if stream == nil {
		vlog.Warningln("handle recv message, missing stream: ", msg.Header.RequestId)
	} else {
		stream.notify(msg)
	}
	return nil
}

func (c *Channel) closeOnErr(err error) {
	c.shutdownLock.Lock()
	if c.shutdownErr == nil {
		c.shutdownErr = err
	}
	if c.shutdown != true { // not normal close
		vlog.Warningf("motan channel will close. err: %s\n", err.Error())
		c.shutdownLock.Unlock()
		c.Close()
	}
}

func (c *Channel) Close() error {
	c.shutdownLock.Lock()
	defer c.shutdownLock.Unlock()
	if c.shutdown {
		return nil
	}
	c.shutdown = true
	close(c.shutdownCh)
	c.conn.Close()
	return nil
}

type ConnFactory func() (net.Conn, error)

type ChannelPool struct {
	channels     chan *Channel
	channelsLock sync.Mutex
	factory      ConnFactory
	config       *Config
}

func (c *ChannelPool) getChannels() chan *Channel {
	c.channelsLock.Lock()
	defer c.channelsLock.Unlock()
	channels := c.channels
	return channels
}

func (c *ChannelPool) Get() (*Channel, error) {
	channels := c.getChannels()
	if channels == nil {
		return nil, errors.New("channels is nil")
	}
	channel, ok := <-channels
	if ok && (channel == nil || channel.IsClosed()) {
		conn, err := c.factory()
		if err != nil {
			vlog.Errorln("create channel failed.", err)
		}
		channel = buildChannel(conn, c.config)
	}
	if err := retChannelPool(channels, channel); err != nil && channel != nil {
		channel.closeOnErr(err)
	}
	if channel == nil {
		return nil, errors.New("channel is nil")
	} else {
		return channel, nil
	}
}

func retChannelPool(channels chan *Channel, channel *Channel) (error error) {
	defer func() {
		if err := recover(); err != nil {
			error = errors.New("ChannelPool has been closed!")
		}
	}()
	if channels == nil {
		return errors.New("channels is nil")
	}
	channels <- channel
	return nil
}

func (c *ChannelPool) Close() error {
	c.channelsLock.Lock()
	channels := c.channels
	c.channels = nil
	c.factory = nil
	c.config = nil
	c.channelsLock.Unlock()
	if channels == nil {
		return nil
	}
	close(channels)
	for channel := range channels {
		if channel != nil {
			channel.Close()
		}
	}
	return nil
}

func NewChannelPool(poolCap int, factory ConnFactory, config *Config) (*ChannelPool, error) {
	if poolCap <= 0 {
		return nil, errors.New("invalid capacity settings")
	}
	channelPool := &ChannelPool{
		channels: make(chan *Channel, poolCap),
		factory:  factory,
		config:   config,
	}
	for i := 0; i < poolCap; i++ {
		conn, err := factory()
		if err != nil {
			channelPool.Close()
			return nil, errors.New("channel pool init failed.")
		}
		channelPool.channels <- buildChannel(conn, config)
	}
	return channelPool, nil
}

func buildChannel(conn net.Conn, config *Config) *Channel {
	if conn == nil {
		return nil
	}
	if config == nil {
		config = DefaultConfig()
	}
	if err := VerifyConfig(config); err != nil {
		return nil
	}
	channel := &Channel{
		conn:       conn,
		config:     config,
		bufRead:    bufio.NewReader(conn),
		sendCh:     make(chan sendReady, 64),
		streams:    make(map[uint64]*Stream, 64),
		heartbeats: make(map[uint64]*Stream),
		shutdownCh: make(chan struct{}),
	}

	go channel.recv()

	go channel.send()

	return channel
}

func GenerateRequestId() uint64 {
	ms := uint64(time.Now().UnixNano() / int64(time.Millisecond))
	offset := atomic.AddUint64(&idOffset, 1) & (1<<20 - 1)
	return ms + offset
}
