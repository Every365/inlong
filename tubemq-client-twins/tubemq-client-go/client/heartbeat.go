/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 * <p>
 * http://www.apache.org/licenses/LICENSE-2.0
 * <p>
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package client

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/incubator-inlong/tubemq-client-twins/tubemq-client-go/errs"
	"github.com/apache/incubator-inlong/tubemq-client-twins/tubemq-client-go/metadata"
	"github.com/apache/incubator-inlong/tubemq-client-twins/tubemq-client-go/protocol"
	"github.com/apache/incubator-inlong/tubemq-client-twins/tubemq-client-go/util"
)

type heartbeatMetadata struct {
	numConnections int
	timer          *time.Timer
}

type heartbeatManager struct {
	consumer   *consumer
	heartbeats map[string]*heartbeatMetadata
	mu         sync.Mutex
}

func newHBManager(consumer *consumer) *heartbeatManager {
	return &heartbeatManager{
		consumer:   consumer,
		heartbeats: make(map[string]*heartbeatMetadata),
	}
}

func (h *heartbeatManager) registerMaster(address string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.heartbeats[address]; !ok {
		h.heartbeats[address] = &heartbeatMetadata{
			numConnections: 1,
			timer:          time.AfterFunc(h.consumer.config.Heartbeat.Interval/2, h.consumerHB2Master),
		}
	}
	hm := h.heartbeats[address]
	hm.numConnections++
}

func (h *heartbeatManager) registerBroker(broker *metadata.Node) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.heartbeats[broker.GetAddress()]; !ok {
		h.heartbeats[broker.GetAddress()] = &heartbeatMetadata{
			numConnections: 1,
			timer:          time.AfterFunc(h.consumer.config.Heartbeat.Interval, func() { h.consumerHB2Broker(broker) }),
		}
	}
	hm := h.heartbeats[broker.GetAddress()]
	hm.numConnections++
}

func (h *heartbeatManager) consumerHB2Master() {
	if time.Now().UnixNano()/int64(time.Millisecond)-h.consumer.lastMasterHb > 30000 {
		h.consumer.rmtDataCache.HandleExpiredPartitions(h.consumer.config.Consumer.MaxConfirmWait)
	}
	m := &metadata.Metadata{}
	node := &metadata.Node{}
	node.SetHost(util.GetLocalHost())
	node.SetAddress(h.consumer.master.Address)
	m.SetNode(node)
	sub := &metadata.SubscribeInfo{}
	sub.SetGroup(h.consumer.config.Consumer.Group)
	m.SetSubscribeInfo(sub)
	h.consumer.unreportedTimes++
	if h.consumer.unreportedTimes > h.consumer.config.Consumer.MaxSubInfoReportInterval {
		m.SetReportTimes(true)
		h.consumer.unreportedTimes = 0
	}

	retry := 0
	for retry < h.consumer.config.Heartbeat.MaxRetryTimes {
		ctx, cancel := context.WithTimeout(context.Background(), h.consumer.config.Net.ReadTimeout)
		rsp, err := h.consumer.client.HeartRequestC2M(ctx, m, h.consumer.subInfo, h.consumer.rmtDataCache)
		if err != nil {
			cancel()
		}
		if rsp.GetSuccess() {
			cancel()
			h.processHBResponseM2C(rsp)
			break
		} else if rsp.GetErrCode() == errs.RetErrHBNoNode || strings.Index(rsp.GetErrMsg(), "StandbyException") != -1 {
			cancel()
			h.consumer.masterHBRetry++
			address := h.consumer.master.Address
			go h.consumer.register2Master(rsp.GetErrCode() != errs.RetErrHBNoNode)
			if rsp.GetErrCode() != errs.RetErrHBNoNode {
				hm := h.heartbeats[address]
				hm.numConnections--
				if hm.numConnections == 0 {
					h.mu.Lock()
					delete(h.heartbeats, address)
					h.mu.Unlock()
				}
			}
			return
		}
		cancel()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	hm := h.heartbeats[h.consumer.master.Address]
	hm.timer.Reset(h.nextHeartbeatInterval())
}

func (h *heartbeatManager) processHBResponseM2C(rsp *protocol.HeartResponseM2C) {
	h.consumer.masterHBRetry = 0
	h.consumer.subInfo.SetIsNotAllocated(rsp.GetNotAllocated())
	if rsp.GetDefFlowCheckId() != 0 || rsp.GetGroupFlowCheckId() != 0 {
		if rsp.GetDefFlowCheckId() != 0 {
			h.consumer.rmtDataCache.UpdateDefFlowCtrlInfo(rsp.GetDefFlowCheckId(), rsp.GetDefFlowControlInfo())
		}
		qryPriorityID := h.consumer.rmtDataCache.GetQryPriorityID()
		if rsp.GetQryPriorityId() != 0 {
			qryPriorityID = rsp.GetQryPriorityId()
		}
		h.consumer.rmtDataCache.UpdateGroupFlowCtrlInfo(qryPriorityID, rsp.GetGroupFlowCheckId(), rsp.GetGroupFlowControlInfo())
	}
	if rsp.GetAuthorizedInfo() != nil {
		h.consumer.processAuthorizedToken(rsp.GetAuthorizedInfo())
	}
	if rsp.GetRequireAuth() {
		atomic.StoreInt32(&h.consumer.nextAuth2Master, 1)
	}
	if rsp.GetEvent() != nil {
		event := rsp.GetEvent()
		subscribeInfo := make([]*metadata.SubscribeInfo, 0, len(event.GetSubscribeInfo()))
		for _, sub := range event.GetSubscribeInfo() {
			s, err := metadata.NewSubscribeInfo(sub)
			if err != nil {
				continue
			}
			subscribeInfo = append(subscribeInfo, s)
		}
		e := metadata.NewEvent(event.GetRebalanceId(), event.GetOpType(), subscribeInfo)
		h.consumer.rmtDataCache.OfferEvent(e)
	}
}

func (h *heartbeatManager) nextHeartbeatInterval() time.Duration {
	interval := h.consumer.config.Heartbeat.Interval
	if h.consumer.masterHBRetry >= h.consumer.config.Heartbeat.MaxRetryTimes {
		interval = h.consumer.config.Heartbeat.AfterFail
	}
	return interval
}

func (h *heartbeatManager) consumerHB2Broker(broker *metadata.Node) {
	h.mu.Lock()
	defer h.mu.Unlock()

	partitions := h.consumer.rmtDataCache.GetPartitionByBroker(broker)
	if len(partitions) == 0 {
		h.resetBrokerTimer(broker)
		return
	}
	m := &metadata.Metadata{}
	m.SetReadStatus(h.consumer.getConsumeReadStatus(false))
	m.SetNode(broker)
	ctx, cancel := context.WithTimeout(context.Background(), h.consumer.config.Net.ReadTimeout)
	defer cancel()

	rsp, err := h.consumer.client.HeartbeatRequestC2B(ctx, m, h.consumer.subInfo, h.consumer.rmtDataCache)
	if err != nil {
		return
	}
	if rsp.GetSuccess() {
		if rsp.GetHasPartFailure() {
			partitionKeys := make([]string, 0, len(rsp.GetFailureInfo()))
			for _, fi := range rsp.GetFailureInfo() {
				pos := strings.Index(fi, ":")
				if pos == -1 {
					continue
				}
				partition, err := metadata.NewPartition(fi[pos+1:])
				if err != nil {
					continue
				}
				partitionKeys = append(partitionKeys, partition.GetPartitionKey())
			}
			h.consumer.rmtDataCache.RemovePartition(partitionKeys)
		} else {
			if rsp.GetErrCode() == errs.RetCertificateFailure {
				partitionKeys := make([]string, 0, len(partitions))
				for _, partition := range partitions {
					partitionKeys = append(partitionKeys, partition.GetPartitionKey())
				}
				h.consumer.rmtDataCache.RemovePartition(partitionKeys)
			}
		}
	}
	h.resetBrokerTimer(broker)
}

func (h *heartbeatManager) resetBrokerTimer(broker *metadata.Node) {
	interval := h.consumer.config.Heartbeat.Interval
	partitions := h.consumer.rmtDataCache.GetPartitionByBroker(broker)
	if len(partitions) == 0 {
		delete(h.heartbeats, broker.GetAddress())
	} else {
		hm := h.heartbeats[broker.GetAddress()]
		hm.timer.Reset(interval)
	}
}
