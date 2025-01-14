// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafsar

import (
	"container/list"
	"context"
	"fmt"
	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/paashzj/kafka_go_pulsar/pkg/constant"
	"github.com/paashzj/kafka_go_pulsar/pkg/network"
	"github.com/paashzj/kafka_go_pulsar/pkg/utils"
	"github.com/pkg/errors"
	"github.com/protocol-laboratory/kafka-codec-go/codec"
	"github.com/sirupsen/logrus"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Broker struct {
	server             Server
	kafkaServer        *network.Server
	pulsarConfig       PulsarConfig
	pulsarCommonClient pulsar.Client
	pulsarClientManage map[string]pulsar.Client
	groupCoordinator   GroupCoordinator
	kafsarConfig       KafsarConfig
	readerManager      map[string]*ReaderMetadata
	mutex              sync.RWMutex
	userInfoManager    map[string]*userInfo
	offsetManager      OffsetManager
	memberManager      map[string]*MemberInfo
	topicGroupManager  map[string]string
	producerManager    map[string]pulsar.Producer
	tracer             NoErrorTracer // common tracer
}

type userInfo struct {
	username string
	clientId string
}

type MessageIdPair struct {
	MessageId pulsar.MessageID
	Offset    int64
}

type MemberInfo struct {
	memberId        string
	groupId         string
	groupInstanceId *string
	clientId        string
}

func NewKafsar(impl Server, config *Config) (*Broker, error) {
	broker := Broker{server: impl, pulsarConfig: config.PulsarConfig, kafsarConfig: config.KafsarConfig}
	pulsarUrl := fmt.Sprintf("pulsar://%s:%d", broker.pulsarConfig.Host, broker.pulsarConfig.TcpPort)
	var err error
	pulsarClient, err := pulsar.NewClient(pulsar.ClientOptions{URL: pulsarUrl})
	if err != nil {
		return nil, err
	}
	pulsarAddr := broker.getPulsarHttpUrl()
	broker.offsetManager, err = NewOffsetManager(pulsarClient, config.KafsarConfig, pulsarAddr)
	if err != nil {
		pulsarClient.Close()
		return nil, err
	}

	offsetChannel := broker.offsetManager.Start()
	for {
		if <-offsetChannel {
			break
		}
	}
	if broker.kafsarConfig.GroupCoordinatorType == Cluster {
		broker.groupCoordinator = NewGroupCoordinatorCluster()
	} else if broker.kafsarConfig.GroupCoordinatorType == Standalone {
		broker.groupCoordinator = NewGroupCoordinatorStandalone(broker.pulsarConfig, broker.kafsarConfig, pulsarClient)
	} else {
		return nil, errors.Errorf("unexpect GroupCoordinatorType: %v", broker.kafsarConfig.GroupCoordinatorType)
	}
	broker.pulsarCommonClient = pulsarClient
	broker.readerManager = make(map[string]*ReaderMetadata)
	broker.userInfoManager = make(map[string]*userInfo)
	broker.memberManager = make(map[string]*MemberInfo)
	broker.pulsarClientManage = make(map[string]pulsar.Client)
	broker.topicGroupManager = make(map[string]string)
	broker.producerManager = make(map[string]pulsar.Producer)
	kfkProtocolConfig := &network.KafkaProtocolConfig{}
	kfkProtocolConfig.ClusterId = config.KafsarConfig.ClusterId
	kfkProtocolConfig.AdvertiseHost = config.KafsarConfig.AdvertiseHost
	kfkProtocolConfig.AdvertisePort = config.KafsarConfig.AdvertisePort
	kfkProtocolConfig.NeedSasl = config.KafsarConfig.NeedSasl
	kfkProtocolConfig.MaxConn = config.KafsarConfig.MaxConn
	var aux network.KafsarServer = &broker
	broker.kafkaServer, err = network.NewServer(&config.KafsarConfig.GnetConfig, kfkProtocolConfig, aux)
	if err != nil {
		return nil, err
	}
	if config.TraceConfig == nil {
		config.TraceConfig = &SkywalkingTracerConfig{}
	}
	broker.tracer = config.TraceConfig
	broker.tracer.NewProvider()
	return &broker, nil
}

func (b *Broker) Run() error {
	logrus.Info("kafsar started")
	return b.kafkaServer.Run()
}

func (b *Broker) Produce(addr net.Addr, kafkaTopic string, partition int, req *codec.ProducePartitionReq) (*codec.ProducePartitionResp, error) {
	span := b.tracer.NewSpan(context.Background(), "Produce", "broker produce msg starting")
	b.tracer.SetAttribute(span, "action", "Produce")
	defer b.tracer.EndSpan(span, fmt.Sprintf("produce msg %s:%d", kafkaTopic, partition))
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("user not exist. username: %s, kafkaTopic: %s", user.username, kafkaTopic)
		return &codec.ProducePartitionResp{
			ErrorCode: codec.TOPIC_AUTHORIZATION_FAILED,
		}, nil
	}
	producer, err := b.getProducer(addr, user.username, kafkaTopic)
	if err != nil {
		logrus.Errorf("create producer failed. username: %s, kafkaTopic: %s", user.username, kafkaTopic)
		return &codec.ProducePartitionResp{
			ErrorCode: codec.TOPIC_AUTHORIZATION_FAILED,
		}, nil
	}
	batch := req.RecordBatch.Records
	count := int32(0)
	producerChan := make(chan bool)
	var offset int64
	for _, kafkaMsg := range batch {
		message := pulsar.ProducerMessage{}
		message.Payload = kafkaMsg.Value
		if kafkaMsg.Key != nil {
			message.Key = string(kafkaMsg.Key)
		}
		producer.SendAsync(context.Background(), &message, func(id pulsar.MessageID, message *pulsar.ProducerMessage, err error) {
			atomic.AddInt32(&count, 1)
			if err != nil {
				logrus.Errorf("send msg failed. username: %s, kafkaTopic: %s, err: %s", user.username, kafkaTopic, err)
			}
			if count == int32(len(batch)) {
				offset = ConvertMsgId(id)
				producerChan <- true
			}
		})
	}
	<-producerChan
	return &codec.ProducePartitionResp{
		PartitionId:     partition,
		Offset:          offset,
		Time:            -1,
		RecordErrorList: nil,
		LogStartOffset:  0,
	}, nil

}

func (b *Broker) Fetch(addr net.Addr, req *codec.FetchReq) ([]*codec.FetchTopicResp, error) {
	traceSpan := b.tracer.NewSpan(context.Background(), "Fetch", "broker fetch action starting")
	b.tracer.SetAttribute(traceSpan, "action", "Fetch")
	var maxWaitTime int
	if req.MaxWaitTime < b.kafsarConfig.MaxFetchWaitMs {
		maxWaitTime = req.MaxWaitTime
	} else {
		maxWaitTime = b.kafsarConfig.MaxFetchWaitMs
	}
	reqList := req.TopicReqList
	result := make([]*codec.FetchTopicResp, len(reqList))
	for i, topicReq := range reqList {
		topicSpan := b.tracer.NewSubSpan(traceSpan, "FetchPartition")
		f := &codec.FetchTopicResp{}
		f.Topic = topicReq.Topic
		f.PartitionRespList = make([]*codec.FetchPartitionResp, len(topicReq.PartitionReqList))
		for j, partitionReq := range topicReq.PartitionReqList {
			f.PartitionRespList[j] = b.FetchPartition(addr, topicReq.Topic, req.ClientId, partitionReq,
				req.MaxBytes, req.MinBytes, maxWaitTime/len(topicReq.PartitionReqList), topicSpan)
		}
		result[i] = f
		b.tracer.EndSpan(topicSpan, fmt.Sprintf("topic: %s fetched", topicReq.Topic))
	}
	b.tracer.EndSpan(traceSpan, "fetch action done")
	return result, nil
}

// FetchPartition visible for testing
func (b *Broker) FetchPartition(addr net.Addr, kafkaTopic, clientID string, req *codec.FetchPartitionReq, maxBytes int, minBytes int, maxWaitMs int, span LocalSpan) *codec.FetchPartitionResp {
	fetchSpan := b.tracer.NewSubSpan(span, fmt.Sprintf("fetching partition %s:%d", kafkaTopic, req.PartitionId))
	defer b.tracer.EndSpan(fetchSpan, fmt.Sprintf("fetched partition %s:%d", kafkaTopic, req.PartitionId))
	start := time.Now()
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	records := make([]*codec.Record, 0)
	recordBatch := codec.RecordBatch{Records: records}
	if !exist {
		logrus.Errorf("fetch partition failed when get userinfo by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &codec.FetchPartitionResp{
			PartitionIndex: req.PartitionId,
			ErrorCode:      codec.UNKNOWN_SERVER_ERROR,
			RecordBatch:    &recordBatch,
		}
	}
	logrus.Infof("%s fetch topic: %s partition %d", addr.String(), kafkaTopic, req.PartitionId)
	partitionedTopic, err := b.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("fetch partition failed when get pulsar topic %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &codec.FetchPartitionResp{
			PartitionIndex: req.PartitionId,
			ErrorCode:      codec.UNKNOWN_SERVER_ERROR,
			RecordBatch:    &recordBatch,
		}
	}
	b.mutex.RLock()
	readerMetadata, exist := b.readerManager[partitionedTopic+clientID]
	if !exist {
		groupId, exist := b.topicGroupManager[partitionedTopic]
		b.mutex.RUnlock()
		if exist {
			group, err := b.groupCoordinator.GetGroup(user.username, groupId)
			if err == nil && group.groupStatus != Stable {
				logrus.Infof("group is preparing rebalance. grouId: %s, topic: %s", groupId, partitionedTopic)
				return &codec.FetchPartitionResp{
					LastStableOffset: 0,
					ErrorCode:        codec.NONE,
					LogStartOffset:   0,
					RecordBatch:      &recordBatch,
					PartitionIndex:   req.PartitionId,
				}
			}
		}
		// Maybe this partition-topic is already assigned to another member
		logrus.Warnf("can not find reader for topic: %s when fetch partition %s", partitionedTopic, partitionedTopic+clientID)
		return &codec.FetchPartitionResp{
			LastStableOffset: 0,
			ErrorCode:        codec.NONE,
			LogStartOffset:   0,
			RecordBatch:      &recordBatch,
			PartitionIndex:   req.PartitionId,
		}
	}
	b.mutex.RUnlock()
	byteLength := 0
	var baseOffset int64
	fistMessage := true
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxWaitMs)*time.Millisecond)
	defer cancel()
OUT:
	for {
		if time.Since(start).Milliseconds() >= int64(maxWaitMs) || len(recordBatch.Records) >= b.kafsarConfig.MaxFetchRecord {
			break OUT
		}
		flowControl := b.server.HasFlowQuota(user.username, partitionedTopic)
		if !flowControl {
			break
		}
		message, err := readerMetadata.reader.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break OUT
			}
			logrus.Errorf("read msg failed. err: %s", err)
			continue
		}
		byteLength = byteLength + utils.CalculateMsgLength(message)
		logrus.Infof("receive msg: %s from %s", message.ID(), message.Topic())
		offset := convOffset(message, b.kafsarConfig.ContinuousOffset)
		if fistMessage {
			fistMessage = false
			baseOffset = offset
		}
		relativeOffset := offset - baseOffset
		record := codec.Record{
			Value:          message.Payload(),
			RelativeOffset: int(relativeOffset),
		}
		recordBatch.Records = append(recordBatch.Records, &record)
		readerMetadata.mutex.Lock()
		readerMetadata.messageIds.PushBack(MessageIdPair{
			MessageId: message.ID(),
			Offset:    offset,
		})
		readerMetadata.mutex.Unlock()
		if byteLength > minBytes && time.Since(start).Milliseconds() >= int64(b.kafsarConfig.MinFetchWaitMs) {
			break
		}
		if byteLength > maxBytes {
			break
		}
	}
	recordBatch.Offset = baseOffset
	return &codec.FetchPartitionResp{
		ErrorCode:        codec.NONE,
		PartitionIndex:   req.PartitionId,
		LastStableOffset: 0,
		LogStartOffset:   0,
		RecordBatch:      &recordBatch,
	}
}

func (b *Broker) getProducer(addr net.Addr, username string, topic string) (pulsar.Producer, error) {
	pulsarTopic, err := b.server.PulsarTopic(username, topic)
	if err != nil {
		logrus.Errorf("get pulsar topic failed. username: %s, topic: %s", username, topic)
		return nil, err
	}
	b.mutex.Lock()
	producer, exist := b.producerManager[addr.String()]
	if !exist {
		options := pulsar.ProducerOptions{}
		options.Topic = pulsarTopic
		options.MaxPendingMessages = b.kafsarConfig.MaxProducerRecordSize
		options.BatchingMaxSize = uint(b.kafsarConfig.MaxBatchSize)
		producer, err = b.pulsarCommonClient.CreateProducer(options)
		if err != nil {
			b.mutex.Unlock()
			logrus.Errorf("crate producer failed. topic: %s, err: %s", pulsarTopic, err)
			return nil, err
		}
		logrus.Infof("create producer success. addr: %s", addr.String())
		b.producerManager[addr.String()] = producer
	}
	b.mutex.Unlock()
	return producer, nil
}

func (b *Broker) GroupJoin(addr net.Addr, req *codec.JoinGroupReq) (*codec.JoinGroupResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("username not found in join group: %s", req.GroupId)
		return &codec.JoinGroupResp{
			ErrorCode:    codec.UNKNOWN_SERVER_ERROR,
			MemberId:     req.MemberId,
			GenerationId: -1,
		}, nil
	}
	logrus.Infof("%s joining to group: %s, memberId: %s", addr.String(), req.GroupId, req.MemberId)
	joinGroupResp, err := b.groupCoordinator.HandleJoinGroup(user.username, req.GroupId, req.MemberId, req.ClientId, req.ProtocolType,
		req.SessionTimeout, req.GroupProtocols)
	if err != nil {
		logrus.Errorf("unexpected exception in join group: %s, error: %s", req.GroupId, err)
		return &codec.JoinGroupResp{
			ErrorCode:    codec.UNKNOWN_SERVER_ERROR,
			MemberId:     req.MemberId,
			GenerationId: -1,
		}, nil
	}
	memberInfo := MemberInfo{
		memberId:        joinGroupResp.MemberId,
		groupId:         req.GroupId,
		groupInstanceId: req.GroupInstanceId,
		clientId:        req.ClientId,
	}
	b.mutex.Lock()
	b.memberManager[addr.String()] = &memberInfo
	b.mutex.Unlock()
	return joinGroupResp, nil
}

func (b *Broker) GroupLeave(addr net.Addr, req *codec.LeaveGroupReq) (*codec.LeaveGroupResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("username not found in leave group: %s", req.GroupId)
		return &codec.LeaveGroupResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s leaving group: %s, members: %+v", addr.String(), req.GroupId, req.Members)
	leaveGroupResp, err := b.groupCoordinator.HandleLeaveGroup(user.username, req.GroupId, req.Members)
	if err != nil {
		logrus.Errorf("unexpected exception in leaving group: %s, error: %s", req.GroupId, err)
		return &codec.LeaveGroupResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	group, err := b.groupCoordinator.GetGroup(user.username, req.GroupId)
	if err != nil {
		logrus.Errorf("get group %s failed, error: %s", req.GroupId, err)
		return &codec.LeaveGroupResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	for _, topic := range group.partitionedTopic {
		b.mutex.Lock()
		readerMetadata, exist := b.readerManager[topic+req.ClientId]
		if exist {
			readerMetadata.reader.Close()
			logrus.Infof("success close reader topic: %s", group.partitionedTopic)
			delete(b.readerManager, topic+req.ClientId)
			readerMetadata = nil
		}
		client, exist := b.pulsarClientManage[topic+req.ClientId]
		if exist {
			client.Close()
			delete(b.pulsarClientManage, topic+req.ClientId)
			client = nil
		}
		delete(b.topicGroupManager, topic)
		b.mutex.Unlock()
	}
	return leaveGroupResp, nil
}

func (b *Broker) GroupSync(addr net.Addr, req *codec.SyncGroupReq) (*codec.SyncGroupResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("username not found in sync group: %s", req.GroupId)
		return &codec.SyncGroupResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s syncing group: %s, memberId: %s", addr.String(), req.GroupId, req.MemberId)
	syncGroupResp, err := b.groupCoordinator.HandleSyncGroup(user.username, req.GroupId, req.MemberId, req.GenerationId, req.GroupAssignments)
	if err != nil {
		logrus.Errorf("unexpected exception in sync group: %s, error: %s", req.GroupId, err)
		return &codec.SyncGroupResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	syncGroupResp.ProtocolName = req.ProtocolName
	syncGroupResp.ProtocolType = req.ProtocolType
	return syncGroupResp, nil
}

func (b *Broker) OffsetListPartition(addr net.Addr, kafkaTopic, clientID string, req *codec.ListOffsetsPartition) (*codec.ListOffsetsPartitionResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset list failed when get username by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &codec.ListOffsetsPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s offset list topic: %s, partition: %d", addr.String(), kafkaTopic, req.PartitionId)
	partitionedTopic, err := b.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("get topic failed. err: %s", err)
		return &codec.ListOffsetsPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	b.mutex.RLock()
	client, exist := b.pulsarClientManage[partitionedTopic+clientID]
	if !exist {
		groupId, exist := b.topicGroupManager[partitionedTopic]
		b.mutex.RUnlock()
		if exist {
			group, err := b.groupCoordinator.GetGroup(user.username, groupId)
			if err == nil && group.groupStatus != Stable {
				logrus.Infof("group is preparing rebalance. grouId: %s, topic: %s", groupId, partitionedTopic)
				return &codec.ListOffsetsPartitionResp{
					PartitionId: req.PartitionId,
					ErrorCode:   codec.LEADER_NOT_AVAILABLE,
					Timestamp:   constant.TimeEarliest,
				}, nil
			}
		}
		logrus.Errorf("get pulsar client failed. err: %v", err)
		return &codec.ListOffsetsPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
			Timestamp:   constant.TimeEarliest,
		}, nil
	}
	readerMessages, exist := b.readerManager[partitionedTopic+clientID]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset list failed, topic: %s, does not exist", partitionedTopic)
		return &codec.ListOffsetsPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	offset := constant.DefaultOffset
	if req.Time == constant.TimeLasted {
		msg, err := utils.GetLatestMsgId(partitionedTopic, b.getPulsarHttpUrl())
		if err != nil {
			logrus.Errorf("get topic %s latest offset failed %s\n", kafkaTopic, err)
			return &codec.ListOffsetsPartitionResp{
				PartitionId: req.PartitionId,
				ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		lastedMsg, err := utils.ReadLastedMsg(partitionedTopic, b.kafsarConfig.MaxFetchWaitMs, msg, client)
		if err != nil {
			logrus.Errorf("read lasted msg failed. topic: %s, err: %s", kafkaTopic, err)
			return &codec.ListOffsetsPartitionResp{
				PartitionId: req.PartitionId,
				ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		if lastedMsg != nil {
			err := readerMessages.reader.Seek(lastedMsg.ID())
			if err != nil {
				logrus.Errorf("offset list failed, topic: %s, err: %s", partitionedTopic, err)
				return &codec.ListOffsetsPartitionResp{
					PartitionId: req.PartitionId,
					ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
				}, nil
			}
			offset = convOffset(lastedMsg, b.kafsarConfig.ContinuousOffset)
		}
	}
	return &codec.ListOffsetsPartitionResp{
		PartitionId: req.PartitionId,
		Offset:      offset,
		Timestamp:   constant.TimeEarliest,
	}, nil
}

func (b *Broker) OffsetCommitPartition(addr net.Addr, kafkaTopic, clientID string, req *codec.OffsetCommitPartitionReq) (*codec.OffsetCommitPartitionResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset commit failed when get userinfo by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &codec.OffsetCommitPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	partitionedTopic, err := b.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("offset commit failed when get pulsar topic %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &codec.OffsetCommitPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	b.mutex.RLock()
	readerMessages, exist := b.readerManager[partitionedTopic+clientID]
	if !exist {
		groupId, exist := b.topicGroupManager[partitionedTopic]
		b.mutex.RUnlock()
		if exist {
			group, err := b.groupCoordinator.GetGroup(user.username, groupId)
			if err == nil && group.groupStatus != Stable {
				logrus.Warnf("group is preparing rebalance. groupId: %s, topic: %s", groupId, partitionedTopic)
				return &codec.OffsetCommitPartitionResp{ErrorCode: codec.REBALANCE_IN_PROGRESS}, nil
			}
		}
		logrus.Warnf("commit offset failed, topic: %s, does not exist", partitionedTopic)
		return &codec.OffsetCommitPartitionResp{ErrorCode: codec.REBALANCE_IN_PROGRESS}, nil
	}
	b.mutex.RUnlock()
	readerMessages.mutex.RLock()
	length := readerMessages.messageIds.Len()
	readerMessages.mutex.RUnlock()
	for i := 0; i < length; i++ {
		readerMessages.mutex.RLock()
		front := readerMessages.messageIds.Front()
		readerMessages.mutex.RUnlock()
		if front == nil {
			break
		}
		messageIdPair := front.Value.(MessageIdPair)
		// kafka commit offset maybe greater than current offset
		if messageIdPair.Offset == req.Offset || ((messageIdPair.Offset < req.Offset) && (i == length-1)) {
			err := b.offsetManager.CommitOffset(user.username, kafkaTopic, readerMessages.groupId, req.PartitionId, messageIdPair)
			if err != nil {
				logrus.Errorf("commit offset failed. topic: %s, err: %s", kafkaTopic, err)
				return &codec.OffsetCommitPartitionResp{
					PartitionId: req.PartitionId,
					ErrorCode:   codec.UNKNOWN_SERVER_ERROR,
				}, nil
			}
			logrus.Infof("ack pulsar %s for %s", partitionedTopic, messageIdPair.MessageId)
			readerMessages.mutex.Lock()
			readerMessages.messageIds.Remove(front)
			readerMessages.mutex.Unlock()
			break
		}
		if messageIdPair.Offset > req.Offset {
			break
		}
		readerMessages.mutex.Lock()
		readerMessages.messageIds.Remove(front)
		readerMessages.mutex.Unlock()
	}
	return &codec.OffsetCommitPartitionResp{
		PartitionId: req.PartitionId,
		ErrorCode:   codec.NONE,
	}, nil
}

func (b *Broker) OffsetFetch(addr net.Addr, topic, clientID, groupID string, req *codec.OffsetFetchPartitionReq) (*codec.OffsetFetchPartitionResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset fetch failed when get userinfo by addr %s, kafka topic: %s", addr.String(), topic)
		return &codec.OffsetFetchPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s fetch topic: %s offset, partition: %d", addr.String(), topic, req.PartitionId)
	partitionedTopic, err := b.partitionedTopic(user, topic, req.PartitionId)
	if err != nil {
		logrus.Errorf("offset fetch failed when get pulsar topic %s, kafka topic: %s", addr.String(), topic)
		return &codec.OffsetFetchPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	subscriptionName, err := b.server.SubscriptionName(groupID)
	if err != nil {
		logrus.Errorf("sync group %s failed when offset fetch, error: %s", groupID, err)
		return &codec.OffsetFetchPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	messagePair, flag := b.offsetManager.AcquireOffset(user.username, topic, groupID, req.PartitionId)
	messageId := pulsar.EarliestMessageID()
	kafkaOffset := constant.UnknownOffset
	if flag {
		kafkaOffset = messagePair.Offset
		messageId = messagePair.MessageId
	}
	b.mutex.RLock()
	_, exist = b.readerManager[partitionedTopic+clientID]
	b.mutex.RUnlock()
	if !exist {
		b.mutex.Lock()
		metadata := ReaderMetadata{groupId: groupID, messageIds: list.New()}
		channel, reader, err := b.createReader(partitionedTopic, subscriptionName, messageId, clientID)
		if err != nil {
			b.mutex.Unlock()
			logrus.Errorf("%s, create channel failed, error: %s", topic, err)
			return &codec.OffsetFetchPartitionResp{
				ErrorCode: codec.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		metadata.reader = reader
		metadata.channel = channel
		b.readerManager[partitionedTopic+clientID] = &metadata
		b.mutex.Unlock()
	}
	group, err := b.groupCoordinator.GetGroup(user.username, groupID)
	if err != nil {
		logrus.Errorf("get group %s failed, error: %s", groupID, err)
		return &codec.OffsetFetchPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	if !b.checkPartitionTopicExist(group.partitionedTopic, partitionedTopic) {
		group.partitionedTopic = append(group.partitionedTopic, partitionedTopic)
	}
	b.mutex.Lock()
	b.topicGroupManager[partitionedTopic] = group.groupId
	b.mutex.Unlock()

	return &codec.OffsetFetchPartitionResp{
		PartitionId: req.PartitionId,
		Offset:      kafkaOffset,
		LeaderEpoch: -1,
		Metadata:    nil,
		ErrorCode:   codec.NONE,
	}, nil
}

func (b *Broker) partitionedTopic(user *userInfo, kafkaTopic string, partitionId int) (string, error) {
	pulsarTopic, err := b.server.PulsarTopic(user.username, kafkaTopic)
	if err != nil {
		return "", err
	}
	return pulsarTopic + fmt.Sprintf(constant.PartitionSuffixFormat, partitionId), nil
}

func (b *Broker) OffsetLeaderEpoch(addr net.Addr, topic string, req *codec.OffsetLeaderEpochPartitionReq) (*codec.OffsetForLeaderEpochPartitionResp, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset fetch failed when get userinfo by addr %s, kafka topic: %s", addr.String(), topic)
		return &codec.OffsetForLeaderEpochPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s offset leader epoch topic: %s, partition: %d", addr.String(), topic, req.PartitionId)
	partitionedTopic, err := b.partitionedTopic(user, topic, req.PartitionId)
	if err != nil {
		logrus.Errorf("get partitioned topic failed. topic: %s", topic)
		return &codec.OffsetForLeaderEpochPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	msgByte, err := utils.GetLatestMsgId(partitionedTopic, b.getPulsarHttpUrl())
	if err != nil {
		logrus.Errorf("get last msgId failed. topic: %s", topic)
		return &codec.OffsetForLeaderEpochPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	msg, err := utils.ReadLastedMsg(partitionedTopic, b.kafsarConfig.MaxFetchWaitMs, msgByte, b.pulsarCommonClient)
	if err != nil {
		logrus.Errorf("get last msgId failed. topic: %s", topic)
		return &codec.OffsetForLeaderEpochPartitionResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	offset := convOffset(msg, b.kafsarConfig.ContinuousOffset)
	return &codec.OffsetForLeaderEpochPartitionResp{
		ErrorCode:   codec.NONE,
		PartitionId: req.PartitionId,
		LeaderEpoch: req.LeaderEpoch,
		Offset:      offset,
	}, nil
}

func (b *Broker) SaslAuth(addr net.Addr, req codec.SaslAuthenticateReq) (bool, codec.ErrorCode) {
	auth, err := b.server.Auth(req.Username, req.Password, req.ClientId)
	if err != nil || !auth {
		return false, codec.SASL_AUTHENTICATION_FAILED
	}
	b.mutex.RLock()
	_, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		b.mutex.Lock()
		b.userInfoManager[addr.String()] = &userInfo{
			username: req.Username,
			clientId: req.ClientId,
		}
		b.mutex.Unlock()
	}
	return true, codec.NONE
}

func (b *Broker) SaslAuthTopic(addr net.Addr, req codec.SaslAuthenticateReq, topic, permissionType string) (bool, codec.ErrorCode) {
	auth, err := b.server.AuthTopic(req.Username, req.Password, req.ClientId, topic, permissionType)
	if err != nil || !auth {
		return false, codec.SASL_AUTHENTICATION_FAILED
	}
	return true, codec.NONE
}

func (b *Broker) SaslAuthConsumerGroup(addr net.Addr, req codec.SaslAuthenticateReq, consumerGroup string) (bool, codec.ErrorCode) {
	auth, err := b.server.AuthTopicGroup(req.Username, req.Password, req.ClientId, consumerGroup)
	if err != nil || !auth {
		return false, codec.SASL_AUTHENTICATION_FAILED
	}
	return true, codec.NONE
}

func (b *Broker) Disconnect(addr net.Addr) {
	logrus.Infof("lost connection: %s", addr)
	if addr == nil {
		return
	}
	b.mutex.RLock()
	memberInfo, exist := b.memberManager[addr.String()]
	producer, producerExist := b.producerManager[addr.String()]
	b.mutex.RUnlock()
	if producerExist {
		producer.Close()
		b.mutex.Lock()
		delete(b.producerManager, addr.String())
		b.mutex.Unlock()
	}
	if !exist {
		b.mutex.Lock()
		delete(b.userInfoManager, addr.String())
		b.mutex.Unlock()
		return
	}
	memberList := []*codec.LeaveGroupMember{
		{
			MemberId:        memberInfo.memberId,
			GroupInstanceId: memberInfo.groupInstanceId,
		},
	}
	req := codec.LeaveGroupReq{
		BaseReq: codec.BaseReq{ClientId: memberInfo.clientId},
		GroupId: memberInfo.groupId,
		Members: memberList,
	}
	_, err := b.GroupLeave(addr, &req)
	if err != nil {
		logrus.Errorf("leave group failed. err: %s", err)
	}
	// leave group will use user information
	b.mutex.Lock()
	delete(b.userInfoManager, addr.String())
	b.mutex.Unlock()
}

func (b *Broker) Close() {
	b.kafkaServer.Close(context.Background())
	b.offsetManager.Close()
	b.mutex.Lock()
	for key, value := range b.pulsarClientManage {
		value.Close()
		delete(b.pulsarClientManage, key)
	}
	for key, value := range b.producerManager {
		value.Close()
		delete(b.producerManager, key)
	}
	b.mutex.Unlock()
}

func (b *Broker) GetOffsetManager() OffsetManager {
	return b.offsetManager
}

func (b *Broker) createReader(partitionedTopic string, subscriptionName string, messageId pulsar.MessageID, clientId string) (chan pulsar.ReaderMessage, pulsar.Reader, error) {
	client, exist := b.pulsarClientManage[partitionedTopic+clientId]
	if !exist {
		var err error
		pulsarUrl := fmt.Sprintf("pulsar://%s:%d", b.pulsarConfig.Host, b.pulsarConfig.TcpPort)
		client, err = pulsar.NewClient(pulsar.ClientOptions{URL: pulsarUrl})
		if err != nil {
			logrus.Errorf("create pulsar client failed.")
			return nil, nil, err
		}
		b.pulsarClientManage[partitionedTopic+clientId] = client
	}
	channel := make(chan pulsar.ReaderMessage, b.kafsarConfig.ConsumerReceiveQueueSize)
	options := pulsar.ReaderOptions{
		Topic:             partitionedTopic,
		Name:              subscriptionName,
		SubscriptionName:  subscriptionName,
		StartMessageID:    messageId,
		MessageChannel:    channel,
		ReceiverQueueSize: b.kafsarConfig.ConsumerReceiveQueueSize,
	}
	reader, err := client.CreateReader(options)
	if err != nil {
		return nil, nil, err
	}
	return channel, reader, nil
}

func (b *Broker) HeartBeat(addr net.Addr, req codec.HeartbeatReq) *codec.HeartbeatResp {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("HeartBeat failed when get userinfo by addr %s", addr.String())
		return &codec.HeartbeatResp{
			ErrorCode: codec.UNKNOWN_SERVER_ERROR,
		}
	}
	resp := b.groupCoordinator.HandleHeartBeat(user.username, req.GroupId, req.MemberId)
	if resp.ErrorCode == codec.REBALANCE_IN_PROGRESS {
		group, err := b.groupCoordinator.GetGroup(user.username, req.GroupId)
		if err != nil {
			logrus.Errorf("HeartBeat failed when get group by addr %s", addr.String())
			return resp
		}
		for _, topic := range group.partitionedTopic {
			b.mutex.Lock()
			readerMetadata, exist := b.readerManager[topic+req.ClientId]
			if exist {
				readerMetadata.reader.Close()
				logrus.Infof("success close reader topic by heartbeat rebalance: %s", group.partitionedTopic)
				delete(b.readerManager, topic+req.ClientId)
				readerMetadata = nil
			}
			client, exist := b.pulsarClientManage[topic+req.ClientId]
			if exist {
				client.Close()
				delete(b.pulsarClientManage, topic+req.ClientId)
				client = nil
			}
			b.mutex.Unlock()
		}
	}
	return resp
}

func (b *Broker) PartitionNum(addr net.Addr, kafkaTopic string) (int, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("get partitionNum failed. user is not found. topic: %s", kafkaTopic)
		return 0, errors.New("user not found.")
	}
	num, err := b.server.PartitionNum(user.username, kafkaTopic)
	if err != nil {
		logrus.Errorf("get partition num failed. topic: %s, err: %s", kafkaTopic, err)
		return 0, errors.New("get partition num failed.")
	}
	return num, nil
}

func (b *Broker) TopicList(addr net.Addr) ([]string, error) {
	b.mutex.RLock()
	user, exist := b.userInfoManager[addr.String()]
	b.mutex.RUnlock()
	if !exist {
		logrus.Errorf("get topics list failed. user not found. addr: %s", addr.String())
		return nil, errors.New("user not found")
	}
	topic, err := b.server.ListTopic(user.username)
	if err != nil {
		logrus.Errorf("get topic list failed. err: %s", err)
		return nil, err
	}
	return topic, nil
}

func (b *Broker) getPulsarHttpUrl() string {
	return fmt.Sprintf("http://%s:%d", b.pulsarConfig.Host, b.pulsarConfig.HttpPort)
}

func (b *Broker) checkPartitionTopicExist(topics []string, partitionTopic string) bool {
	for _, topic := range topics {
		if strings.EqualFold(topic, partitionTopic) {
			return true
		}
	}
	return false
}
