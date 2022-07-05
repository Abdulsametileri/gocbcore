package gocbcore

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/couchbase/gocbcore/v10/memd"
)

type syncClient struct {
	client *memdBootstrapClient
}

func (client *syncClient) SupportsFeature(feature memd.HelloFeature) bool {
	return client.client.SupportsFeature(feature)
}

func (client *syncClient) Address() string {
	return client.client.Address()
}

func (client *syncClient) doBasicOp(cmd memd.CmdCode, k, v, e []byte, deadline time.Time) ([]byte, error) {
	return client.client.SendSyncRequest(
		&memd.Packet{
			Magic:   memd.CmdMagicReq,
			Command: cmd,
			Key:     k,
			Value:   v,
			Extras:  e,
		},
		deadline,
	)
}

func (client *syncClient) ExecDcpControl(key string, value string, deadline time.Time) error {
	_, err := client.doBasicOp(memd.CmdDcpControl, []byte(key), []byte(value), nil, deadline)
	return err
}

func (client *syncClient) ExecGetClusterConfig(deadline time.Time) ([]byte, error) {
	return client.doBasicOp(memd.CmdGetClusterConfig, nil, nil, nil, deadline)
}

func (client *syncClient) ExecOpenDcpConsumer(streamName string, openFlags memd.DcpOpenFlag, deadline time.Time) error {
	extraBuf := make([]byte, 8)
	binary.BigEndian.PutUint32(extraBuf[0:], 0)
	binary.BigEndian.PutUint32(extraBuf[4:], uint32((openFlags & ^memd.DcpOpenFlag(3))|memd.DcpOpenFlagProducer))
	_, err := client.doBasicOp(memd.CmdDcpOpenConnection, []byte(streamName), nil, extraBuf, deadline)
	return err
}

func (client *syncClient) ExecEnableDcpNoop(period time.Duration, deadline time.Time) error {
	// The client will always reply to No-Op's.  No need to enable it

	err := client.ExecDcpControl("enable_noop", "true", deadline)
	if err != nil {
		return err
	}

	periodStr := fmt.Sprintf("%d", period/time.Second)
	err = client.ExecDcpControl("set_noop_interval", periodStr, deadline)
	if err != nil {
		return err
	}

	return nil
}

func (client *syncClient) ExecEnableDcpClientEnd(deadline time.Time) error {
	memcli, ok := client.client.client.(*memdClient)
	if !ok {
		return errCliInternalError
	}

	err := client.ExecDcpControl("send_stream_end_on_client_close_stream", "true", deadline)
	if err != nil {
		memcli.streamEndNotSupported = true
	}

	return nil
}

func (client *syncClient) ExecEnableDcpBufferAck(bufferSize int, deadline time.Time) error {
	mclient, ok := client.client.client.(*memdClient)
	if !ok {
		return errCliInternalError
	}

	// Enable buffer acknowledgment on the client
	mclient.EnableDcpBufferAck(bufferSize / 2)

	bufferSizeStr := fmt.Sprintf("%d", bufferSize)
	err := client.ExecDcpControl("connection_buffer_size", bufferSizeStr, deadline)
	if err != nil {
		return err
	}

	return nil
}
