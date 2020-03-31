package gocbcore

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/couchbase/gocbcore/v8/memd"
)

type diagnosticsComponent struct {
	kvMux  *kvMux
	bucket string
}

func newDianosticsComponent(kvMux *kvMux, bucket string) *diagnosticsComponent {
	return &diagnosticsComponent{
		kvMux:  kvMux,
		bucket: bucket,
	}
}

func (dc *diagnosticsComponent) PingKv(opts PingKvOptions, cb PingKvCallback) (PendingOp, error) {
	iter, err := dc.kvMux.PipelineSnapshot()
	if err != nil {
		return nil, err
	}

	op := &pingOp{
		callback:  cb,
		remaining: 1,
		configRev: iter.RevID(),
	}

	pingStartTime := time.Now()

	bucketName := ""
	if dc.bucket != "" {
		bucketName = redactMetaData(dc.bucket)
	}

	addrToID := make(map[string]string)

	kvHandler := func(resp *memdQResponse, req *memdQRequest, err error) {
		serverAddress := resp.sourceAddr

		pingLatency := time.Now().Sub(pingStartTime)

		op.lock.Lock()
		id := addrToID[serverAddress]
		op.results = append(op.results, PingResult{
			Endpoint: serverAddress,
			Error:    err,
			Latency:  pingLatency,
			Scope:    bucketName,
			ID:       id,
		})
		op.handledOneLocked()
		op.lock.Unlock()
	}

	retryStrat := newFailFastRetryStrategy()

	iter.Iterate(0, func(pipeline *memdPipeline) bool {
		serverAddress := pipeline.Address()

		req := &memdQRequest{
			Packet: memd.Packet{
				Magic:    memd.CmdMagicReq,
				Command:  memd.CmdNoop,
				Datatype: 0,
				Cas:      0,
				Key:      nil,
				Value:    nil,
			},
			Callback:      kvHandler,
			RetryStrategy: retryStrat,
		}

		curOp, err := dc.kvMux.DispatchDirectToAddress(req, pipeline)
		if err != nil {
			op.lock.Lock()
			op.results = append(op.results, PingResult{
				Endpoint: redactSystemData(serverAddress),
				Error:    err,
				Latency:  0,
				Scope:    bucketName,
			})
			op.lock.Unlock()
			return false
		}

		op.lock.Lock()
		op.subops = append(op.subops, pingSubOp{
			endpoint: serverAddress,
			op:       curOp,
		})
		atomic.AddInt32(&op.remaining, 1)
		addrToID[serverAddress] = fmt.Sprintf("%p", pipeline)
		op.lock.Unlock()

		// We iterate through all pipelines
		return false
	})

	// We initialized remaining to one to ensure that the callback is not
	// invoked until all of the operations have been dispatched first.  This
	// final handling is to indicate that all operations were dispatched.
	op.lock.Lock()
	op.handledOneLocked()
	op.lock.Unlock()

	return op, nil
}

// Diagnostics returns diagnostics information about the client.
// Mainly containing a list of open connections and their current
// states.
func (dc *diagnosticsComponent) Diagnostics() (*DiagnosticInfo, error) {
	for {
		iter, err := dc.kvMux.PipelineSnapshot()
		if err != nil {
			return nil, err
		}

		var conns []MemdConnInfo

		iter.Iterate(0, func(pipeline *memdPipeline) bool {
			pipeline.clientsLock.Lock()
			for _, pipecli := range pipeline.clients {
				localAddr := ""
				remoteAddr := ""
				var lastActivity time.Time

				pipecli.lock.Lock()
				if pipecli.client != nil {
					localAddr = pipecli.client.LocalAddress()
					remoteAddr = pipecli.client.Address()
					lastActivityUs := atomic.LoadInt64(&pipecli.client.lastActivity)
					if lastActivityUs != 0 {
						lastActivity = time.Unix(0, lastActivityUs)
					}
				}
				pipecli.lock.Unlock()

				conn := MemdConnInfo{
					LocalAddr:    localAddr,
					RemoteAddr:   remoteAddr,
					LastActivity: lastActivity,
					ID:           fmt.Sprintf("%p", pipecli),
				}
				if dc.bucket != "" {
					conn.Scope = redactMetaData(dc.bucket)
				}
				conns = append(conns, conn)
			}
			pipeline.clientsLock.Unlock()
			return false
		})

		endIter, err := dc.kvMux.PipelineSnapshot()
		if err != nil {
			return nil, err
		}
		if iter.RevID() == endIter.RevID() {
			return &DiagnosticInfo{
				ConfigRev: iter.RevID(),
				MemdConns: conns,
			}, nil
		}
	}
}