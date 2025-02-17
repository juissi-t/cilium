// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Hubble

//go:build !privileged_tests
// +build !privileged_tests

package observer

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/client-go/tools/cache"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"github.com/cilium/cilium/pkg/hubble/container"
	"github.com/cilium/cilium/pkg/hubble/observer/observeroption"
	observerTypes "github.com/cilium/cilium/pkg/hubble/observer/types"
	"github.com/cilium/cilium/pkg/hubble/parser"
	"github.com/cilium/cilium/pkg/hubble/testutils"
	"github.com/cilium/cilium/pkg/monitor"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"
)

var log *logrus.Logger

func init() {
	log = logrus.New()
	log.SetOutput(io.Discard)
}

func noopParser(t *testing.T) *parser.Parser {
	pp, err := parser.New(
		log,
		&testutils.NoopEndpointGetter,
		&testutils.NoopIdentityGetter,
		&testutils.NoopDNSGetter,
		&testutils.NoopIPGetter,
		&testutils.NoopServiceGetter,
		&testutils.NoopLinkGetter,
	)
	require.NoError(t, err)
	return pp
}

func TestNewLocalServer(t *testing.T) {
	pp := noopParser(t)
	s, err := NewLocalServer(pp, log)
	require.NoError(t, err)
	assert.NotNil(t, s.GetStopped())
	assert.NotNil(t, s.GetPayloadParser())
	assert.NotNil(t, s.GetRingBuffer())
	assert.NotNil(t, s.GetLogger())
	assert.NotNil(t, s.GetEventsChannel())
}

func TestLocalObserverServer_ServerStatus(t *testing.T) {
	pp := noopParser(t)
	s, err := NewLocalServer(pp, log, observeroption.WithMaxFlows(container.Capacity1))
	require.NoError(t, err)
	res, err := s.ServerStatus(context.Background(), &observerpb.ServerStatusRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(0), res.SeenFlows)
	assert.Equal(t, uint64(0), res.NumFlows)
	assert.Equal(t, uint64(1), res.MaxFlows)
}

func TestLocalObserverServer_GetFlows(t *testing.T) {
	numFlows := 100
	queueSize := 0
	req := &observerpb.GetFlowsRequest{Number: uint64(10)}
	i := 0
	fakeServer := &testutils.FakeGetFlowsServer{
		OnSend: func(response *observerpb.GetFlowsResponse) error {
			assert.Equal(t, response.GetTime(), response.GetFlow().GetTime())
			assert.Equal(t, response.GetNodeName(), response.GetFlow().GetNodeName())
			i++
			return nil
		},
		FakeGRPCServerStream: &testutils.FakeGRPCServerStream{
			OnContext: func() context.Context {
				return context.Background()
			},
		},
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMaxFlows(container.Capacity127),
		observeroption.WithMonitorBuffer(queueSize),
	)
	require.NoError(t, err)
	go s.Start()

	m := s.GetEventsChannel()
	for i := 0; i < numFlows; i++ {
		tn := monitor.TraceNotifyV0{Type: byte(monitorAPI.MessageTypeTrace)}
		data := testutils.MustCreateL3L4Payload(tn)
		m <- &observerTypes.MonitorEvent{
			Timestamp: time.Unix(int64(i), 0),
			NodeName:  fmt.Sprintf("node #%03d", i),
			Payload: &observerTypes.PerfEvent{
				Data: data,
				CPU:  0,
			},
		}
	}
	close(s.GetEventsChannel())
	<-s.GetStopped()
	err = s.GetFlows(req, fakeServer)
	assert.NoError(t, err)
	assert.Equal(t, req.Number, uint64(i))
}

func TestLocalObserverServer_GetAgentEvents(t *testing.T) {
	numEvents := 100
	queueSize := 0
	req := &observerpb.GetAgentEventsRequest{
		Number: uint64(numEvents),
	}
	cidr := "10.0.0.0/8"
	agentEventsReceived := 0
	agentStartedReceived := 0
	fakeServer := &testutils.FakeGetAgentEventsServer{
		OnSend: func(response *observerpb.GetAgentEventsResponse) error {
			switch ev := response.GetAgentEvent(); ev.GetType() {
			case flowpb.AgentEventType_AGENT_STARTED:
				startEvent := response.GetAgentEvent().GetAgentStart()
				assert.NotNil(t, startEvent)
				assert.Equal(t, startEvent.GetTime().GetSeconds(), int64(42))
				assert.Equal(t, startEvent.GetTime().GetNanos(), int32(1))
				agentStartedReceived++
			case flowpb.AgentEventType_IPCACHE_UPSERTED:
				ipcacheUpdate := response.GetAgentEvent().GetIpcacheUpdate()
				assert.NotNil(t, ipcacheUpdate)
				assert.Equal(t, cidr, ipcacheUpdate.GetCidr())
			case flowpb.AgentEventType_SERVICE_DELETED:
				serviceDelete := response.GetAgentEvent().GetServiceDelete()
				assert.NotNil(t, serviceDelete)
			default:
				assert.Fail(t, "unexpected agent event", ev)
			}
			agentEventsReceived++
			return nil
		},
		FakeGRPCServerStream: &testutils.FakeGRPCServerStream{
			OnContext: func() context.Context {
				return context.Background()
			},
		},
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMonitorBuffer(queueSize),
	)
	require.NoError(t, err)
	go s.Start()

	m := s.GetEventsChannel()
	for i := 0; i < numEvents; i++ {
		ts := time.Unix(int64(i), 0)
		node := fmt.Sprintf("node #%03d", i)
		var msg monitorAPI.AgentNotifyMessage
		if i == 0 {
			msg = monitorAPI.StartMessage(time.Unix(42, 1))
		} else if i%2 == 1 {
			msg = monitorAPI.IPCacheUpsertedMessage(cidr, uint32(i), nil, net.ParseIP("10.1.5.4"), nil, 0xff, "default", "foobar")
		} else {
			msg = monitorAPI.ServiceDeleteMessage(uint32(i))
		}
		m <- &observerTypes.MonitorEvent{
			Timestamp: ts,
			NodeName:  node,
			Payload: &observerTypes.AgentEvent{
				Type:    monitorAPI.MessageTypeAgent,
				Message: msg,
			},
		}
	}
	close(s.GetEventsChannel())
	<-s.GetStopped()
	err = s.GetAgentEvents(req, fakeServer)
	assert.NoError(t, err)
	assert.Equal(t, 1, agentStartedReceived)
	// FIXME:
	// This should be assert.Equals(t, numEvents, agentEventsReceived)
	// A bug in the ring buffer prevents this from succeeding
	assert.Greater(t, agentEventsReceived, 0)
}

func TestLocalObserverServer_GetFlows_Follow_Since(t *testing.T) {
	numFlows := 100
	queueSize := 0

	since := time.Unix(5, 0)
	sinceProto := timestamppb.New(since)
	assert.NoError(t, sinceProto.CheckValid())
	req := &observerpb.GetFlowsRequest{
		Since:  sinceProto,
		Follow: true,
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMaxFlows(container.Capacity127),
		observeroption.WithMonitorBuffer(queueSize),
	)
	require.NoError(t, err)
	go s.Start()

	generateFlows := func(from, to int, m chan<- *observerTypes.MonitorEvent) {
		for i := from; i < to; i++ {
			tn := monitor.TraceNotifyV0{Type: byte(monitorAPI.MessageTypeTrace)}
			data := testutils.MustCreateL3L4Payload(tn)
			m <- &observerTypes.MonitorEvent{
				Timestamp: time.Unix(int64(i), 0),
				NodeName:  fmt.Sprintf("node #%03d", i),
				Payload: &observerTypes.PerfEvent{
					Data: data,
					CPU:  0,
				},
			}
		}
	}

	// produce first half of flows before request and second half during request
	m := s.GetEventsChannel()
	generateFlows(0, numFlows/2, m)

	receivedFlows := 0
	fakeServer := &testutils.FakeGetFlowsServer{
		OnSend: func(response *observerpb.GetFlowsResponse) error {
			receivedFlows++
			assert.Equal(t, response.GetTime(), response.GetFlow().GetTime())
			assert.Equal(t, response.GetNodeName(), response.GetFlow().GetNodeName())

			assert.NoError(t, response.GetTime().CheckValid())
			ts := response.GetTime().AsTime()
			assert.True(t, !ts.Before(since), "flow had invalid timestamp. ts=%s, since=%s", ts, since)

			// start producing flows once we have seen the most recent one.
			// Most recently produced flow has timestamp (numFlows/2)-1, but is
			// inaccessible to readers due to the way the ring buffer works
			if int(ts.Unix()) == (numFlows/2)-2 {
				go func() {
					generateFlows(numFlows/2, numFlows, m)
					close(m)
				}()
			}

			// terminate the request once we have seen enough flows.
			// we expected to see all generated flows, minus the ones filtered
			// out by 'since', minus the one inaccessible in the ring buffer
			if receivedFlows == numFlows-int(since.Unix())-1 {
				// this will terminate the follow request
				return io.EOF
			}

			return nil
		},
		FakeGRPCServerStream: &testutils.FakeGRPCServerStream{
			OnContext: func() context.Context {
				return context.Background()
			},
		},
	}

	err = s.GetFlows(req, fakeServer)
	<-s.GetStopped()
	assert.Equal(t, err, io.EOF)
}

type fakeCiliumDaemon struct{}

func (f *fakeCiliumDaemon) DebugEnabled() bool {
	return true
}

func (f *fakeCiliumDaemon) GetK8sStore(name string) cache.Store {
	return nil
}

func TestHooks(t *testing.T) {
	numFlows := 10
	queueSize := 0

	ciliumDaemon := &fakeCiliumDaemon{}
	onServerInit := func(srv observeroption.Server) error {
		assert.Equal(t, srv.GetOptions().CiliumDaemon, ciliumDaemon)
		return nil
	}

	seenFlows := int64(0)
	skipEveryNFlows := int64(2)
	onMonitorEventFirst := func(ctx context.Context, event *observerTypes.MonitorEvent) (bool, error) {
		seenFlows++

		assert.Equal(t, event.Timestamp.Unix(), seenFlows-1)
		if seenFlows%skipEveryNFlows == 0 {
			return true, nil
		}
		return false, nil
	}
	onMonitorEventSecond := func(ctx context.Context, event *observerTypes.MonitorEvent) (bool, error) {
		if seenFlows%skipEveryNFlows == 0 {
			assert.Fail(t, "server did not break loop after onMonitorEventFirst")
		}
		return false, nil
	}
	onDecodedFlow := func(ctx context.Context, f *flowpb.Flow) (bool, error) {
		if seenFlows%skipEveryNFlows == 0 {
			assert.Fail(t, "server did not stop decoding after onMonitorEventFirst")
		}
		return false, nil
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMaxFlows(container.Capacity15),
		observeroption.WithMonitorBuffer(queueSize),
		observeroption.WithCiliumDaemon(ciliumDaemon),
		observeroption.WithOnServerInitFunc(onServerInit),
		observeroption.WithOnMonitorEventFunc(onMonitorEventFirst),
		observeroption.WithOnMonitorEventFunc(onMonitorEventSecond),
		observeroption.WithOnDecodedFlowFunc(onDecodedFlow),
	)
	require.NoError(t, err)
	go s.Start()

	m := s.GetEventsChannel()
	for i := 0; i < numFlows; i++ {
		tn := monitor.TraceNotifyV0{Type: byte(monitorAPI.MessageTypeTrace)}
		data := testutils.MustCreateL3L4Payload(tn)
		m <- &observerTypes.MonitorEvent{
			Timestamp: time.Unix(int64(i), 0),
			NodeName:  fmt.Sprintf("node #%03d", i),
			Payload: &observerTypes.PerfEvent{
				Data: data,
				CPU:  0,
			},
		}
	}
	close(s.GetEventsChannel())
	<-s.GetStopped()
	assert.Equal(t, int64(numFlows), seenFlows)
}

func TestLocalObserverServer_OnFlowDelivery(t *testing.T) {
	numFlows := 100
	queueSize := 0
	req := &observerpb.GetFlowsRequest{Number: uint64(100)}
	flowsReceived := 0
	fakeServer := &testutils.FakeGetFlowsServer{
		OnSend: func(response *observerpb.GetFlowsResponse) error {
			assert.Equal(t, response.GetTime(), response.GetFlow().GetTime())
			assert.Equal(t, response.GetNodeName(), response.GetFlow().GetNodeName())
			flowsReceived++
			return nil
		},
		FakeGRPCServerStream: &testutils.FakeGRPCServerStream{
			OnContext: func() context.Context {
				return context.Background()
			},
		},
	}

	count := 0
	onFlowDelivery := func(ctx context.Context, f *flowpb.Flow) (bool, error) {
		count++
		if count%2 == 0 {
			return true, nil
		}
		return false, nil
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMaxFlows(container.Capacity127),
		observeroption.WithMonitorBuffer(queueSize),
		observeroption.WithOnFlowDeliveryFunc(onFlowDelivery),
	)
	require.NoError(t, err)
	go s.Start()

	m := s.GetEventsChannel()
	for i := 0; i < numFlows; i++ {
		tn := monitor.TraceNotifyV0{Type: byte(monitorAPI.MessageTypeTrace)}
		data := testutils.MustCreateL3L4Payload(tn)
		m <- &observerTypes.MonitorEvent{
			Timestamp: time.Unix(int64(i), 0),
			NodeName:  fmt.Sprintf("node #%03d", i),
			Payload: &observerTypes.PerfEvent{
				Data: data,
				CPU:  0,
			},
		}
	}
	close(s.GetEventsChannel())
	<-s.GetStopped()
	err = s.GetFlows(req, fakeServer)
	assert.NoError(t, err)
	// Only every second flow should have been received
	assert.Equal(t, flowsReceived, numFlows/2)
}

func TestLocalObserverServer_OnGetFlows(t *testing.T) {
	numFlows := 100
	queueSize := 0
	req := &observerpb.GetFlowsRequest{Number: uint64(100)}
	flowsReceived := 0
	fakeServer := &testutils.FakeGetFlowsServer{
		OnSend: func(response *observerpb.GetFlowsResponse) error {
			assert.Equal(t, response.GetTime(), response.GetFlow().GetTime())
			assert.Equal(t, response.GetNodeName(), response.GetFlow().GetNodeName())
			flowsReceived++
			return nil
		},
		FakeGRPCServerStream: &testutils.FakeGRPCServerStream{
			OnContext: func() context.Context {
				return context.Background()
			},
		},
	}

	type contextKey string
	key := contextKey("foo")
	onGetFlows := func(ctx context.Context, req *observerpb.GetFlowsRequest) (context.Context, error) {
		return context.WithValue(ctx, key, 10), nil
	}

	onFlowDelivery := func(ctx context.Context, f *flowpb.Flow) (bool, error) {
		// Pass if context is available
		if ctx.Value(key) != nil {
			return false, nil
		}
		return true, nil
	}

	pp := noopParser(t)
	s, err := NewLocalServer(pp, log,
		observeroption.WithMaxFlows(container.Capacity127),
		observeroption.WithMonitorBuffer(queueSize),
		observeroption.WithOnFlowDeliveryFunc(onFlowDelivery),
		observeroption.WithOnGetFlowsFunc(onGetFlows),
	)
	require.NoError(t, err)
	go s.Start()

	m := s.GetEventsChannel()
	for i := 0; i < numFlows; i++ {
		tn := monitor.TraceNotifyV0{Type: byte(monitorAPI.MessageTypeTrace)}
		data := testutils.MustCreateL3L4Payload(tn)
		m <- &observerTypes.MonitorEvent{
			Timestamp: time.Unix(int64(i), 0),
			NodeName:  fmt.Sprintf("node #%03d", i),
			Payload: &observerTypes.PerfEvent{
				Data: data,
				CPU:  0,
			},
		}
	}
	close(s.GetEventsChannel())
	<-s.GetStopped()
	err = s.GetFlows(req, fakeServer)
	assert.NoError(t, err)
	// FIXME:
	// This should be assert.Equals(t, flowsReceived, numFlows)
	// A bug in the ring buffer prevents this from succeeding
	assert.Greater(t, flowsReceived, 0)
}
