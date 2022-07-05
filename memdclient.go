package gocbcore

import (
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/couchbase/gocbcore/v10/memd"

	"github.com/golang/snappy"
)

func isCompressibleOp(command memd.CmdCode) bool {
	switch command {
	case memd.CmdSet:
		fallthrough
	case memd.CmdAdd:
		fallthrough
	case memd.CmdReplace:
		fallthrough
	case memd.CmdAppend:
		fallthrough
	case memd.CmdPrepend:
		return true
	}
	return false
}

type postCompleteErrorHandler func(resp *memdQResponse, req *memdQRequest, err error) (bool, error)

type memdClient struct {
	lastActivity          int64
	dcpAckSize            int
	dcpFlowRecv           int
	closeNotify           chan bool
	connReleaseNotify     chan struct{}
	connReleasedNotify    chan struct{}
	connID                string
	closed                bool
	conn                  memdConn
	opList                *memdOpMap
	features              []memd.HelloFeature
	lock                  sync.Mutex
	streamEndNotSupported bool
	breaker               circuitBreaker
	postErrHandler        postCompleteErrorHandler
	tracer                *tracerComponent
	zombieLogger          *zombieLoggerComponent

	dcpQueueSize int

	// When a close request comes in, we need to immediately stop processing all requests.  This
	// includes immediately stopping the DCP queue rather than waiting for the application to
	// flush that queue.  This means that we lose packets that were read but not processed, but
	// this is not fundamentally different to if we had just not read them at all.  As a side
	// effect of this, we need to use a separate kill signal on top of closing the queue.
	// We need this to be owned by the client because we only use it when the client is closed,
	// when the connection is closed from an external actor (e.g. server) we want to flush the queue.
	shutdownDCP uint32

	compressionMinSize   int
	compressionMinRatio  float64
	disableDecompression bool

	gracefulCloseTriggered uint32
}

type dcpBuffer struct {
	resp       *memdQResponse
	packetLen  int
	isInternal bool
}

type memdClientProps struct {
	ClientID string

	DCPQueueSize         int
	CompressionMinSize   int
	CompressionMinRatio  float64
	DisableDecompression bool
}

func newMemdClient(props memdClientProps, conn memdConn, breakerCfg CircuitBreakerConfig, postErrHandler postCompleteErrorHandler,
	tracer *tracerComponent, zombieLogger *zombieLoggerComponent) *memdClient {
	client := memdClient{
		closeNotify:        make(chan bool),
		connReleaseNotify:  make(chan struct{}),
		connReleasedNotify: make(chan struct{}),
		connID:             props.ClientID + "/" + formatCbUID(randomCbUID()),
		postErrHandler:     postErrHandler,
		tracer:             tracer,
		zombieLogger:       zombieLogger,
		conn:               conn,
		opList:             newMemdOpMap(),

		dcpQueueSize:         props.DCPQueueSize,
		compressionMinRatio:  props.CompressionMinRatio,
		compressionMinSize:   props.CompressionMinSize,
		disableDecompression: props.DisableDecompression,
	}

	if breakerCfg.Enabled {
		client.breaker = newLazyCircuitBreaker(breakerCfg, client.sendCanary)
	} else {
		client.breaker = newNoopCircuitBreaker()
	}

	client.run()
	return &client
}

func (client *memdClient) SupportsFeature(feature memd.HelloFeature) bool {
	return checkSupportsFeature(client.features, feature)
}

// Features must be set from a context where no racey behaviours can occur, i.e. during bootstrap.
func (client *memdClient) Features(features []memd.HelloFeature) {
	client.features = features

	for _, feature := range features {
		client.conn.EnableFeature(feature)
	}
}

func (client *memdClient) EnableDcpBufferAck(bufferAckSize int) {
	client.dcpAckSize = bufferAckSize
}

func (client *memdClient) maybeSendDcpBufferAck(packetLen int) {
	client.dcpFlowRecv += packetLen
	if client.dcpFlowRecv < client.dcpAckSize {
		return
	}

	ackAmt := client.dcpFlowRecv

	extrasBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(extrasBuf, uint32(ackAmt))

	err := client.conn.WritePacket(&memd.Packet{
		Magic:   memd.CmdMagicReq,
		Command: memd.CmdDcpBufferAck,
		Extras:  extrasBuf,
	})
	if err != nil {
		logWarnf("Failed to dispatch DCP buffer ack: %s", err)
	}

	client.dcpFlowRecv -= ackAmt
}

func (client *memdClient) Address() string {
	return client.conn.RemoteAddr()
}

func (client *memdClient) ConnID() string {
	return client.connID
}

func (client *memdClient) CloseNotify() chan bool {
	return client.closeNotify
}

func (client *memdClient) takeRequestOwnership(req *memdQRequest) error {
	client.lock.Lock()
	defer client.lock.Unlock()

	if client.closed {
		logDebugf("Attempted to put dispatched op OP=0x%x, Opaque=%d in drained opmap", req.Command, req.Opaque)
		return errMemdClientClosed
	}

	if atomic.LoadUint32(&client.gracefulCloseTriggered) == 1 {
		logDebugf("Attempted to dispatch op OP=0x%x, Opaque=%d from gracefully closing memdclient", req.Command, req.Opaque)
		return errMemdClientClosed
	}

	if !atomic.CompareAndSwapPointer(&req.waitingIn, nil, unsafe.Pointer(client)) {
		logDebugf("Attempted to put dispatched op OP=0x%x, Opaque=%d in new opmap", req.Command, req.Opaque)
		return errRequestAlreadyDispatched
	}

	if req.isCancelled() {
		atomic.CompareAndSwapPointer(&req.waitingIn, unsafe.Pointer(client), nil)
		return errRequestCanceled
	}

	connInfo := memdQRequestConnInfo{
		lastDispatchedTo:   client.Address(),
		lastDispatchedFrom: client.conn.LocalAddr(),
		lastConnectionID:   client.connID,
	}
	req.SetConnectionInfo(connInfo)

	client.opList.Add(req)
	return nil
}

func (client *memdClient) CancelRequest(req *memdQRequest, err error) bool {
	client.lock.Lock()
	defer client.lock.Unlock()

	if client.closed {
		logDebugf("Attempted to remove op OP=0x%x, Opaque=%d from drained opmap", req.Command, req.Opaque)
		return false
	}

	removed := client.opList.Remove(req)
	if removed {
		atomic.CompareAndSwapPointer(&req.waitingIn, unsafe.Pointer(client), nil)
	}

	if client.breaker.CompletionCallback(err) {
		client.breaker.MarkSuccessful()
	} else {
		client.breaker.MarkFailure()
	}

	return removed
}

func (client *memdClient) SendRequest(req *memdQRequest) error {
	if !client.breaker.AllowsRequest() {
		logSchedf("Circuit breaker interrupting request. %s to %s OP=0x%x. Opaque=%d", client.conn.LocalAddr(), client.Address(), req.Command, req.Opaque)

		req.cancelWithCallback(errCircuitBreakerOpen)

		return nil
	}

	return client.internalSendRequest(req)
}

func (client *memdClient) internalSendRequest(req *memdQRequest) error {
	if err := client.takeRequestOwnership(req); err != nil {
		return err
	}

	packet := &req.Packet
	if client.SupportsFeature(memd.FeatureSnappy) {
		isCompressed := (packet.Datatype & uint8(memd.DatatypeFlagCompressed)) != 0
		packetSize := len(packet.Value)
		if !isCompressed && packetSize > client.compressionMinSize && isCompressibleOp(packet.Command) {
			compressedValue := snappy.Encode(nil, packet.Value)
			if float64(len(compressedValue))/float64(packetSize) <= client.compressionMinRatio {
				newPacket := *packet
				newPacket.Value = compressedValue
				newPacket.Datatype = newPacket.Datatype | uint8(memd.DatatypeFlagCompressed)
				packet = &newPacket
			}
		}
	}

	logSchedf("Writing request. %s to %s OP=0x%x. Opaque=%d", client.conn.LocalAddr(), client.Address(), req.Command, req.Opaque)

	client.tracer.StartNetTrace(req)

	err := client.conn.WritePacket(packet)
	if err != nil {
		logDebugf("memdClient write failure: %v", err)
		return err
	}

	return nil
}

func (client *memdClient) resolveRequest(resp *memdQResponse) {
	defer memd.ReleasePacket(resp.Packet)

	logSchedf("Handling response data. OP=0x%x. Opaque=%d. Status:%d", resp.Command, resp.Opaque, resp.Status)

	client.lock.Lock()
	// Find the request that goes with this response, don't check if the client is
	// closed so that we can handle orphaned responses.
	req := client.opList.FindAndMaybeRemove(resp.Opaque, resp.Status != memd.StatusSuccess)
	client.lock.Unlock()

	if atomic.LoadUint32(&client.gracefulCloseTriggered) == 1 {
		client.lock.Lock()
		size := client.opList.Size()
		client.lock.Unlock()

		if size == 0 {
			// Let's make sure that we don't somehow slow down returning to the user here.
			go func() {
				// We use the Close function rather than closeConn to ensure that we don't try to close the
				// connection/client if someone else has already closed it.
				err := client.Close()
				if err != nil {
					logDebugf("failed to shutdown memdclient during graceful close: %s", err)
				}
			}()
		}
	}

	if req == nil {
		// There is no known request that goes with this response.  Ignore it.
		logDebugf("Received response with no corresponding request.")
		if client.zombieLogger != nil {
			client.zombieLogger.RecordZombieResponse(resp, client.connID, client.LocalAddress(), client.Address())
		}
		return
	}

	if !req.Persistent || resp.Status != memd.StatusSuccess {
		atomic.CompareAndSwapPointer(&req.waitingIn, unsafe.Pointer(client), nil)
	}

	req.processingLock.Lock()

	if !req.Persistent {
		stopNetTrace(req, resp, client.conn.LocalAddr(), client.conn.RemoteAddr())
	}

	isCompressed := (resp.Datatype & uint8(memd.DatatypeFlagCompressed)) != 0
	if isCompressed && !client.disableDecompression {
		newValue, err := snappy.Decode(nil, resp.Value)
		if err != nil {
			req.processingLock.Unlock()
			logDebugf("Failed to decompress value from the server for key `%s`.", req.Key)
			return
		}

		resp.Value = newValue
		resp.Datatype = resp.Datatype & ^uint8(memd.DatatypeFlagCompressed)
	}

	// Give the agent an opportunity to intercept the response first
	var err error
	if resp.Magic == memd.CmdMagicRes && resp.Status != memd.StatusSuccess {
		err = getKvStatusCodeError(resp.Status)
	}

	if client.breaker.CompletionCallback(err) {
		client.breaker.MarkSuccessful()
	} else {
		client.breaker.MarkFailure()
	}

	if !req.Persistent {
		stopCmdTrace(req)
	}

	req.processingLock.Unlock()

	if err != nil {
		shortCircuited, routeErr := client.postErrHandler(resp, req, err)
		if shortCircuited {
			logSchedf("Routing callback intercepted response")
			return
		}
		err = routeErr
	}

	// Call the requests callback handler...
	logSchedf("Dispatching response callback. OP=0x%x. Opaque=%d", resp.Command, resp.Opaque)
	req.tryCallback(resp, err)
}

func (client *memdClient) run() {
	var (
		// A queue for DCP commands so we can execute them out-of-band from packet receiving.  This
		// is integral to allow the higher level application to back-pressure against the DCP packet
		// processing without interfeering with the SDKs control commands (like config fetch).
		dcpBufferQ = make(chan *dcpBuffer, client.dcpQueueSize)

		// After we signal that DCP processing should stop, we need a notification so we know when
		// it has been completed, we do this to prevent leaving the goroutine around, and we need to
		// ensure that the application has finished with the last packet it received before we stop.
		dcpProcDoneCh = make(chan struct{})
	)

	go func() {
		defer close(dcpProcDoneCh)

		for {
			// If the client has been told to close then we need to finish ASAP, otherwise if the dcpBufferQ has been
			// closed then we'll flush the queue first.
			q, stillOpen := <-dcpBufferQ
			if !stillOpen || atomic.LoadUint32(&client.shutdownDCP) != 0 {
				return
			}

			logSchedf("Resolving response OP=0x%x. Opaque=%d", q.resp.Command, q.resp.Opaque)
			client.resolveRequest(q.resp)

			// See below for information on MB-26363 for why this is here.
			if !q.isInternal && client.dcpAckSize > 0 {
				client.maybeSendDcpBufferAck(q.packetLen)
			}
		}
	}()

	go func() {
		for {
			packet, n, err := client.conn.ReadPacket()
			if err != nil {
				client.lock.Lock()
				if !client.closed {
					logWarnf("memdClient read failure on conn `%v` : %v", client.connID, err)
				}
				client.lock.Unlock()
				break
			}

			resp := &memdQResponse{
				sourceAddr:   client.conn.RemoteAddr(),
				sourceConnID: client.connID,
				Packet:       packet,
			}

			atomic.StoreInt64(&client.lastActivity, time.Now().UnixNano())

			// We handle DCP no-op's directly here so we can reply immediately.
			if resp.Packet.Command == memd.CmdDcpNoop {
				err := client.conn.WritePacket(&memd.Packet{
					Magic:   memd.CmdMagicRes,
					Command: memd.CmdDcpNoop,
					Opaque:  resp.Opaque,
				})
				if err != nil {
					logWarnf("Failed to dispatch DCP noop reply: %s", err)
				}
				continue
			}

			// This is a fix for a bug in the server DCP implementation (MB-26363).  This
			// bug causes the server to fail to send a stream-end notification.  The server
			// does however synchronously stop the stream, and thus we can assume no more
			// packets will be received following the close response.
			if resp.Magic == memd.CmdMagicRes && resp.Command == memd.CmdDcpCloseStream && client.streamEndNotSupported {
				closeReq := client.opList.Find(resp.Opaque)
				if closeReq != nil {
					vbID := closeReq.Vbucket
					streamReq := client.opList.FindOpenStream(vbID)
					if streamReq != nil {
						endExtras := make([]byte, 4)
						binary.BigEndian.PutUint32(endExtras, uint32(memd.StreamEndClosed))
						endResp := &memdQResponse{
							Packet: &memd.Packet{
								Magic:   memd.CmdMagicReq,
								Command: memd.CmdDcpStreamEnd,
								Vbucket: vbID,
								Opaque:  streamReq.Opaque,
								Extras:  endExtras,
							},
						}
						dcpBufferQ <- &dcpBuffer{
							resp:       endResp,
							packetLen:  n,
							isInternal: true,
						}
					}
				}
			}

			switch resp.Packet.Command {
			case memd.CmdDcpDeletion, memd.CmdDcpExpiration, memd.CmdDcpMutation, memd.CmdDcpSnapshotMarker,
				memd.CmdDcpEvent, memd.CmdDcpOsoSnapshot, memd.CmdDcpSeqNoAdvanced, memd.CmdDcpStreamEnd:
				dcpBufferQ <- &dcpBuffer{
					resp:      resp,
					packetLen: n,
				}
			default:
				logSchedf("Resolving response OP=0x%x. Opaque=%d", resp.Command, resp.Opaque)
				client.resolveRequest(resp)
			}
		}

		client.lock.Lock()
		if !client.closed {
			client.closed = true
			client.lock.Unlock()

			err := client.closeConn(true)
			if err != nil {
				// Lets log a warning, as this is non-fatal
				logWarnf("Failed to shut down client connection (%s)", err)
			}
		} else {
			client.lock.Unlock()
		}

		// We close the buffer channel to wake the processor if its asleep (queue was empty).
		// We then wait to ensure it is finished with whatever packet (or packets if the connection was closed by the
		// server) was being processed.
		close(dcpBufferQ)
		<-dcpProcDoneCh

		close(client.connReleaseNotify)

		client.opList.Drain(func(req *memdQRequest) {
			if !atomic.CompareAndSwapPointer(&req.waitingIn, unsafe.Pointer(client), nil) {
				logWarnf("Encountered an unowned request in a client opMap")
			}

			shortCircuited, routeErr := client.postErrHandler(nil, req, io.EOF)
			if shortCircuited {
				return
			}

			req.tryCallback(nil, routeErr)
		})

		<-client.connReleasedNotify

		close(client.closeNotify)
	}()
}

func (client *memdClient) LocalAddress() string {
	return client.conn.LocalAddr()
}

func (client *memdClient) GracefulClose(err error) {
	if atomic.CompareAndSwapUint32(&client.gracefulCloseTriggered, 0, 1) {
		client.lock.Lock()
		if client.closed {
			client.lock.Unlock()
			return
		}
		persistentReqs := client.opList.FindAndRemoveAllPersistent()
		client.lock.Unlock()

		if err == nil {
			err = io.EOF
		}

		for _, req := range persistentReqs {
			req.cancelWithCallback(err)
		}

		// Close down the DCP worker, there can't be any future DCP messages. We don't
		// strictly need to do this, as connection close will trigger it to close anyway.
		atomic.StoreUint32(&client.shutdownDCP, 1)

		client.lock.Lock()
		size := client.opList.Size()
		if size > 0 {
			// If there are items in the op list then we need to go into graceful shutdown mode, so don't close anything
			// yet.
			client.lock.Unlock()
			return
		}

		// If there are no items in the oplist then it's safe to close down the client and connection now.
		if client.closed {
			client.lock.Unlock()
			return
		}
		client.closed = true
		client.lock.Unlock()

		err := client.closeConn(false)
		if err != nil {
			// Lets log a warning, as this is non-fatal
			logWarnf("Failed to shut down client connection (%s)", err)
		}

	}
}

func (client *memdClient) closeConn(internalTrigger bool) error {
	logDebugf("%s/%p memdclient closing connection, internal close: %t", client.Address(), client, internalTrigger)
	if err := client.conn.Close(); err != nil {
		logDebugf("Failed to close memdconn: %v", err)
		client.conn.Release()
		close(client.connReleasedNotify)
		return err
	}

	if !internalTrigger {
		<-client.connReleaseNotify
	}

	client.conn.Release()
	close(client.connReleasedNotify)
	return nil
}

func (client *memdClient) Close() error {
	// We mark that we are shutting down to stop the DCP processor from running any
	// additional packets up to the application. We do this before the closed check to
	// force stop flushing. Rebalance etc... uses GracefulClose so if we received this Close
	// then we do need to shutdown in a timely manner.
	atomic.StoreUint32(&client.shutdownDCP, 1)

	client.lock.Lock()
	if client.closed {
		client.lock.Unlock()
		return nil
	}
	client.closed = true
	client.lock.Unlock()

	return client.closeConn(false)
}

func (client *memdClient) sendCanary() {
	errChan := make(chan error)
	handler := func(resp *memdQResponse, req *memdQRequest, err error) {
		errChan <- err
	}

	req := &memdQRequest{
		Packet: memd.Packet{
			Magic:    memd.CmdMagicReq,
			Command:  memd.CmdNoop,
			Datatype: 0,
			Cas:      0,
			Key:      nil,
			Value:    nil,
		},
		Callback:      handler,
		RetryStrategy: newFailFastRetryStrategy(),
	}

	logDebugf("Sending NOOP request for %p/%s", client, client.Address())
	err := client.internalSendRequest(req)
	if err != nil {
		client.breaker.MarkFailure()
	}

	timer := AcquireTimer(client.breaker.CanaryTimeout())
	select {
	case <-timer.C:
		if !req.internalCancel(errRequestCanceled) {
			err := <-errChan
			if err == nil {
				logDebugf("NOOP request successful for %p/%s", client, client.Address())
				client.breaker.MarkSuccessful()
			} else {
				logDebugf("NOOP request failed for %p/%s", client, client.Address())
				client.breaker.MarkFailure()
			}
		}
		client.breaker.MarkFailure()
	case err := <-errChan:
		if err == nil {
			client.breaker.MarkSuccessful()
		} else {
			client.breaker.MarkFailure()
		}
	}
}
