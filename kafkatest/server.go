package kafkatest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/dropbox/kafka/proto"
)

type topicOffset struct {
	offset   int64
	metadata string
}

// Server is container for fake kafka server data.
type Server struct {
	mu          *sync.RWMutex
	brokers     []proto.MetadataRespBroker
	topics      map[string]map[int32][]*proto.Message
	offsets     map[string]map[int32]map[string]*topicOffset
	ln          net.Listener
	middlewares []Middleware
	started     bool
	stopped     bool
}

// Middleware is function that is called for every incomming kafka message,
// before running default processing handler. Middleware function can return
// nil or kafka response message.
type Middleware func(nodeID int32, requestKind int16, content []byte) Response

// Response is any kafka response as defined in kafka/proto package
type Response interface {
	Bytes() ([]byte, error)
}

// NewServer return new mock server instance. Any number of middlewares can be
// passed to customize request handling. For every incomming request, all
// middlewares are called one after another in order they were passed. If any
// middleware return non nil response message, response is instasntly written
// to the client and no further code execution for the request is made -- no
// other middleware is called nor the default handler is executed.
func NewServer(middlewares ...Middleware) *Server {
	s := &Server{
		brokers:     make([]proto.MetadataRespBroker, 0),
		topics:      make(map[string]map[int32][]*proto.Message),
		offsets:     make(map[string]map[int32]map[string]*topicOffset),
		middlewares: middlewares,
		mu:          &sync.RWMutex{},
	}
	return s
}

// Addr return server instance address or empty string if not running.
func (s *Server) Addr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.stopped {
		panic("server stopped, no addr available")
	} else if !s.started {
		panic("server never started, no addr available")
	}

	if s.ln != nil {
		return s.ln.Addr().String()
	}
	panic("server should be running but isn't, no addr available")
}

// Reset will clear out local messages and topics.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.topics = make(map[string]map[int32][]*proto.Message)
	s.offsets = make(map[string]map[int32]map[string]*topicOffset)
}

// ResetTopic removes all messages and committed offsets for a topic, but
// does not remove the topic or its partitions.
func (s *Server) ResetTopic(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if parts, ok := s.topics[topic]; ok {
		for partitionID := range parts {
			parts[partitionID] = make([]*proto.Message, 0)
		}
	}
	delete(s.offsets, topic)
}

// Close shut down server if running. It is safe to call it more than once.
func (s *Server) Close() (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stopped = true

	if s.ln != nil {
		err = s.ln.Close()
		s.ln = nil
	}
	return err
}

// ServeHTTP provides JSON serialized server state information.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	topics := make(map[string]map[string][]*proto.Message)
	for name, parts := range s.topics {
		topics[name] = make(map[string][]*proto.Message)
		for part, messages := range parts {
			topics[name][strconv.Itoa(int(part))] = messages
		}
	}

	w.Header().Set("content-type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]interface{}{
		"topics":  topics,
		"brokers": s.brokers,
	})
	if err != nil {
		log.Errorf("cannot JSON encode state: %s", err)
	}
}

// AddMessages append messages to given topic/partition. If topic or partition
// does not exists, it is being created.
// To only create topic/partition, call this method withough giving any
// message.
func (s *Server) AddMessages(topic string, partition int32, messages ...*proto.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parts, ok := s.topics[topic]
	if !ok {
		parts = make(map[int32][]*proto.Message)
		s.topics[topic] = parts
	}

	for i := int32(0); i <= partition; i++ {
		if _, ok := parts[i]; !ok {
			parts[i] = make([]*proto.Message, 0)
		}
	}
	if len(messages) > 0 {
		start := len(parts[partition])
		for i, msg := range messages {
			msg.Offset = int64(start + i)
			msg.Partition = partition
			msg.Topic = topic
		}
		parts[partition] = append(parts[partition], messages...)
	}
}

// Run starts kafka mock server listening on given address. Function only
// returns when the listener has exited.
func (s *Server) Run(addr string) error {
	const nodeID = 100

	ln, err := func() (net.Listener, error) {
		s.mu.Lock()
		defer s.mu.Unlock()

		if s.ln != nil {
			log.Errorf("server already running: %s", s.ln.Addr())
			return nil, fmt.Errorf("server already running: %s", s.ln.Addr())
		}

		ln, err := net.Listen("tcp4", addr)
		if err != nil {
			log.Errorf("cannot listen on address %q: %s", addr, err)
			return nil, fmt.Errorf("cannot listen: %s", err)
		}

		s.ln = ln
		s.started = true

		if host, port, err := net.SplitHostPort(ln.Addr().String()); err != nil {
			log.Errorf("cannot extract host/port from %q: %s", ln.Addr(), err)
			return nil, fmt.Errorf("cannot extract host/port from %q: %s", ln.Addr(), err)
		} else {
			prt, err := strconv.Atoi(port)
			if err != nil {
				log.Errorf("invalid port %q: %s", port, err)
				return nil, fmt.Errorf("invalid port %q: %s", port, err)
			}
			s.brokers = append(s.brokers, proto.MetadataRespBroker{
				NodeID: nodeID,
				Host:   host,
				Port:   int32(prt),
			})
		}
		return ln, nil
	}()
	if err != nil {
		return err
	}

	// Defer the stop/close so we shut down properly
	defer s.Close()

	// Handle incoming connections for a long time
	for {
		if conn, err := ln.Accept(); err == nil {
			go s.handleClient(nodeID, conn)
		} else {
			log.Errorf("failed to accept: %s", err)
			return fmt.Errorf("failed to accept: %s", err)
		}
	}
}

// MustSpawn run server in the background on random port. It panics if server
// cannot be spawned.
// Use Close method to stop spawned server.
func (s *Server) MustSpawn() {
	const nodeID = 100

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ln != nil {
		return
	}

	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		panic(fmt.Sprintf("cannot listen: %s", err))
	}
	s.ln = ln
	s.started = true

	if host, port, err := net.SplitHostPort(ln.Addr().String()); err != nil {
		panic(fmt.Sprintf("cannot extract host/port from %q: %s", ln.Addr(), err))
	} else {
		prt, err := strconv.Atoi(port)
		if err != nil {
			panic(fmt.Sprintf("invalid port %q: %s", port, err))
		}
		s.brokers = append(s.brokers, proto.MetadataRespBroker{
			NodeID: nodeID,
			Host:   host,
			Port:   int32(prt),
		})
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err == nil {
				go s.handleClient(nodeID, conn)
			}
		}
	}()
}

func (s *Server) handleClient(nodeID int32, conn net.Conn) {
	defer func() {
		_ = conn.Close()
	}()

	for {
		kind, b, err := proto.ReadReq(conn)
		if err != nil {
			if err != io.EOF {
				log.Errorf("client read error: %s", err)
			}
			return
		}

		var resp response

		for _, middleware := range s.middlewares {
			resp = middleware(nodeID, kind, b)
			if resp != nil {
				break
			}
		}

		if resp == nil {
			switch kind {
			case proto.ProduceReqKind:
				req, err := proto.ReadProduceReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse produce request: %s\n%s", err, b)
					return
				}
				resp = s.handleProduceRequest(nodeID, conn, req)
			case proto.FetchReqKind:
				req, err := proto.ReadFetchReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse fetch request: %s\n%s", err, b)
					return
				}
				resp = s.handleFetchRequest(nodeID, conn, req)
			case proto.OffsetReqKind:
				req, err := proto.ReadOffsetReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse offset request: %s\n%s", err, b)
					return
				}
				resp = s.handleOffsetRequest(nodeID, conn, req)
			case proto.MetadataReqKind:
				req, err := proto.ReadMetadataReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse metadata request: %s\n%s", err, b)
					return
				}
				resp = s.handleMetadataRequest(nodeID, conn, req)
			case proto.OffsetCommitReqKind:
				req, err := proto.ReadOffsetCommitReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse offset commit request: %s\n%s", err, b)
					return
				}
				resp = s.handleOffsetCommitRequest(nodeID, conn, req)
			case proto.OffsetFetchReqKind:
				req, err := proto.ReadOffsetFetchReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse offset fetch request: %s\n%s", err, b)
					return
				}
				resp = s.handleOffsetFetchRequest(nodeID, conn, req)
			case proto.GroupCoordinatorReqKind:
				req, err := proto.ReadGroupCoordinatorReq(bytes.NewBuffer(b))
				if err != nil {
					log.Errorf("cannot parse consumer metadata request: %s\n%s", err, b)
					return
				}
				resp = s.handleGroupCoordinatorRequest(nodeID, conn, req)
			default:
				log.Errorf("unknown request: %d\n%s", kind, b)
				return
			}
		}

		if resp == nil {
			log.Errorf("no response for %d", kind)
			return
		}
		b, err = resp.Bytes()
		if err != nil {
			log.Errorf("cannot serialize %T response: %s", resp, err)
		}
		if _, err := conn.Write(b); err != nil {
			log.Errorf("cannot write %T response: %s", resp, err)
			return
		}
	}
}

type response interface {
	Bytes() ([]byte, error)
}

func (s *Server) handleProduceRequest(
	nodeID int32, conn net.Conn, req *proto.ProduceReq) response {

	s.mu.Lock()
	defer s.mu.Unlock()

	resp := &proto.ProduceResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.ProduceRespTopic, len(req.Topics)),
	}

	for ti, topic := range req.Topics {
		t, ok := s.topics[topic.Name]
		if !ok {
			t = make(map[int32][]*proto.Message)
			s.topics[topic.Name] = t
		}

		respParts := make([]proto.ProduceRespPartition, len(topic.Partitions))
		resp.Topics[ti].Name = topic.Name
		resp.Topics[ti].Partitions = respParts

		for pi, part := range topic.Partitions {
			p, ok := t[part.ID]
			if !ok {
				p = make([]*proto.Message, 0)
				t[part.ID] = p
			}

			log.Infof("produced %d messages to %s:%d at offset %d",
				len(part.Messages), topic.Name, part.ID, len(t[part.ID]))
			for _, msg := range part.Messages {
				msg.Offset = int64(len(t[part.ID]))
				msg.Topic = topic.Name
				t[part.ID] = append(t[part.ID], msg)
			}

			respParts[pi].ID = part.ID
			respParts[pi].Offset = int64(len(t[part.ID])) - 1
		}
	}
	return resp
}

func (s *Server) handleFetchRequest(
	nodeID int32, conn net.Conn, req *proto.FetchReq) response {

	s.mu.RLock()
	defer s.mu.RUnlock()

	resp := &proto.FetchResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.FetchRespTopic, len(req.Topics)),
	}
	for ti, topic := range req.Topics {
		respParts := make([]proto.FetchRespPartition, len(topic.Partitions))
		resp.Topics[ti].Name = topic.Name
		resp.Topics[ti].Partitions = respParts
		for pi, part := range topic.Partitions {
			respParts[pi].ID = part.ID

			partitions, ok := s.topics[topic.Name]
			if !ok {
				respParts[pi].Err = proto.ErrUnknownTopicOrPartition
				continue
			}
			messages, ok := partitions[part.ID]
			if !ok {
				respParts[pi].Err = proto.ErrUnknownTopicOrPartition
				continue
			}
			if part.FetchOffset > int64(len(messages)) {
				respParts[pi].Err = proto.ErrOffsetOutOfRange
				continue
			}
			respParts[pi].TipOffset = int64(len(messages))
			respParts[pi].Messages = messages[part.FetchOffset:]
			numFetched := len(respParts[pi].Messages)
			if numFetched > 0 || !strings.HasPrefix(topic.Name, "__") {
				log.Infof("fetched %d messages from %s:%d at offset %d",
					numFetched, topic.Name, part.ID, part.FetchOffset)
			}
		}
	}

	return resp
}

func (s *Server) handleOffsetRequest(
	nodeID int32, conn net.Conn, req *proto.OffsetReq) response {

	s.mu.RLock()
	defer s.mu.RUnlock()

	resp := &proto.OffsetResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.OffsetRespTopic, len(req.Topics)),
	}
	for ti, topic := range req.Topics {
		respPart := make([]proto.OffsetRespPartition, len(topic.Partitions))
		resp.Topics[ti].Name = topic.Name
		resp.Topics[ti].Partitions = respPart
		for pi, part := range topic.Partitions {
			respPart[pi].ID = part.ID
			switch part.TimeMs {
			case -1: // latest
				msgs := len(s.topics[topic.Name][part.ID])
				respPart[pi].Offsets = []int64{int64(msgs), 0}
				log.Infof("requested latest offset from %s:%d, returning %d",
					topic.Name, part.ID, msgs)
			case -2: // earliest
				respPart[pi].Offsets = []int64{0, 0}
				log.Infof("requested earliest offset from %s:%d, returning %d",
					topic.Name, part.ID, 0)
			default:
				log.Errorf("offset time for %s:%d not supported: %d",
					topic.Name, part.ID, part.TimeMs)
				return nil
			}

			// Now if they've asked for fewer, cut some off -- unclear if this
			// is correct but it seems so given what we support right now
			respPart[pi].Offsets = respPart[pi].Offsets[0:part.MaxOffsets]
		}
	}
	return resp
}

func (s *Server) handleGroupCoordinatorRequest(
	nodeID int32, conn net.Conn, req *proto.GroupCoordinatorReq) response {

	// Fetches the read-lock, so try not to do this inside our own RLock
	// as that can lead to deadlock state if someone happens to try to Lock
	// while we're inside the first RLock
	addr := s.Addr()

	s.mu.RLock()
	defer s.mu.RUnlock()

	log.Infof("requested consumer metadata")

	addrps := strings.Split(addr, ":")
	port, _ := strconv.Atoi(addrps[1])

	return &proto.GroupCoordinatorResp{
		CorrelationID:   req.CorrelationID,
		CoordinatorID:   0,
		CoordinatorHost: addrps[0],
		CoordinatorPort: int32(port),
	}
}

func (s *Server) getTopicOffset(group, topic string, partID int32) *topicOffset {
	pmap, ok := s.offsets[topic]
	if !ok {
		pmap = make(map[int32]map[string]*topicOffset)
		s.offsets[topic] = pmap
	}

	groups, ok := pmap[partID]
	if !ok {
		groups = make(map[string]*topicOffset)
		pmap[partID] = groups
	}

	toffset, ok := groups[group]
	if !ok {
		toffset = &topicOffset{}
		groups[group] = toffset
	}

	return toffset
}

func (s *Server) handleOffsetFetchRequest(
	nodeID int32, conn net.Conn, req *proto.OffsetFetchReq) response {

	s.mu.Lock()
	defer s.mu.Unlock()

	resp := &proto.OffsetFetchResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.OffsetFetchRespTopic, len(req.Topics)),
	}
	for ti, topic := range req.Topics {
		respPart := make([]proto.OffsetFetchRespPartition, len(topic.Partitions))
		resp.Topics[ti].Name = topic.Name
		resp.Topics[ti].Partitions = respPart
		for pi, part := range topic.Partitions {
			toffset := s.getTopicOffset(req.ConsumerGroup, topic.Name, part)
			respPart[pi].ID = part
			respPart[pi].Metadata = toffset.metadata
			respPart[pi].Offset = toffset.offset
			log.Infof("requested committed offset for group %s from %s:%d, returning %d",
				req.ConsumerGroup, topic.Name, part, toffset.offset)
		}
	}
	return resp
}

func (s *Server) handleOffsetCommitRequest(
	nodeID int32, conn net.Conn, req *proto.OffsetCommitReq) response {

	s.mu.Lock()
	defer s.mu.Unlock()

	resp := &proto.OffsetCommitResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.OffsetCommitRespTopic, len(req.Topics)),
	}
	for ti, topic := range req.Topics {
		respPart := make([]proto.OffsetCommitRespPartition, len(topic.Partitions))
		resp.Topics[ti].Name = topic.Name
		resp.Topics[ti].Partitions = respPart
		for pi, part := range topic.Partitions {
			toffset := s.getTopicOffset(req.ConsumerGroup, topic.Name, part.ID)
			toffset.metadata = part.Metadata
			toffset.offset = part.Offset

			respPart[pi].ID = part.ID
			log.Infof("committed offset for group %s from %s:%d, saved %d",
				req.ConsumerGroup, topic.Name, part.ID, part.Offset)
		}
	}
	return resp
}

func (s *Server) handleMetadataRequest(
	nodeID int32, conn net.Conn, req *proto.MetadataReq) response {

	s.mu.Lock()
	defer s.mu.Unlock()

	log.Infof("requested metadata")

	resp := &proto.MetadataResp{
		CorrelationID: req.CorrelationID,
		Topics:        make([]proto.MetadataRespTopic, 0, len(s.topics)),
		Brokers:       s.brokers,
	}

	if req.Topics != nil && len(req.Topics) > 0 {
		// if particular topic was requested, create empty log if does not yet exists
		for _, name := range req.Topics {
			partitions, ok := s.topics[name]
			if !ok {
				partitions = make(map[int32][]*proto.Message)
				partitions[0] = make([]*proto.Message, 0)
				s.topics[name] = partitions
			}

			parts := make([]proto.MetadataRespPartition, len(partitions))
			for pid := range partitions {
				p := &parts[pid]
				p.ID = pid
				p.Leader = nodeID
				p.Replicas = []int32{nodeID}
				p.Isrs = []int32{nodeID}
			}
			resp.Topics = append(resp.Topics, proto.MetadataRespTopic{
				Name:       name,
				Partitions: parts,
			})

		}
	} else {
		for name, partitions := range s.topics {
			parts := make([]proto.MetadataRespPartition, len(partitions))
			for pid := range partitions {
				p := &parts[pid]
				p.ID = pid
				p.Leader = nodeID
				p.Replicas = []int32{nodeID}
				p.Isrs = []int32{nodeID}
			}
			resp.Topics = append(resp.Topics, proto.MetadataRespTopic{
				Name:       name,
				Partitions: parts,
			})
		}
	}
	return resp
}
