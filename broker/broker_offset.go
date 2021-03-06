package broker

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/celrenheit/sandflake"
	"github.com/gogo/protobuf/proto"

	"github.com/celrenheit/sandglass-grpc/go/sgproto"
	"github.com/celrenheit/sandglass/storage"
	"github.com/celrenheit/sandglass/topic"
)

func (b *Broker) Acknowledge(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID) (bool, error) {
	return b.mark(ctx, topicName, partitionName, consumerGroup, consumerName, offset, sgproto.MarkKind_Acknowledged)
}

func (b *Broker) NotAcknowledge(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID) (bool, error) {
	return b.mark(ctx, topicName, partitionName, consumerGroup, consumerName, offset, sgproto.MarkKind_NotAcknowledged)
}

// FIXME: share same code between AcknowledgeMessages a AcknowledgeMessage
func (b *Broker) AcknowledgeMessages(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offsets []sandflake.ID) error {
	topic := b.getTopic(ConsumerOffsetTopicName)
	p := topic.ChoosePartitionForKey(partitionKey(topicName, partitionName, consumerGroup))

	n := b.getPartitionLeader(ConsumerOffsetTopicName, p.Id)
	if n == nil {
		return ErrNoLeaderFound
	}

	if n.Name != b.Name() {
		change := &sgproto.MultiOffsetChangeRequest{
			Topic:         topicName,
			Partition:     partitionName,
			ConsumerGroup: consumerGroup,
			ConsumerName:  consumerName,
			Offsets:       offsets,
		}

		_, err := n.AcknowledgeMessages(ctx, change)

		if err != nil {
			return err
		}

		return nil
	}

	produceRequest := &sgproto.ProduceMessageRequest{
		Topic:     ConsumerOffsetTopicName,
		Partition: p.Id,
	}
	for _, offset := range offsets {
		state := &sgproto.MarkState{
			Kind: sgproto.MarkKind_Acknowledged,
		}

		value, err := proto.Marshal(state)
		if err != nil {
			return err
		}

		produceRequest.Messages = append(produceRequest.Messages, &sgproto.Message{
			Offset:        offset,
			Key:           partitionKey(topicName, partitionName, consumerGroup),
			ClusteringKey: generateClusterKey(offset, sgproto.MarkKind_Acknowledged),
			Value:         value,
		})
	}

	_, err := b.Produce(ctx, produceRequest)
	return err
}

func (b *Broker) Commit(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID) (bool, error) {
	return b.mark(ctx, topicName, partitionName, consumerGroup, consumerName, offset, sgproto.MarkKind_Commited)
}

func (b *Broker) MarkConsumed(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID) (bool, error) {
	return b.mark(ctx, topicName, partitionName, consumerGroup, consumerName, offset, sgproto.MarkKind_Consumed)
}

func (b *Broker) mark(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID, kind sgproto.MarkKind) (bool, error) {
	topic := b.getTopic(ConsumerOffsetTopicName)
	p := topic.ChoosePartitionForKey(partitionKey(topicName, partitionName, consumerGroup))

	n := b.getPartitionLeader(ConsumerOffsetTopicName, p.Id)
	if n == nil {
		return false, ErrNoLeaderFound
	}

	if n.Name != b.Name() {
		change := &sgproto.OffsetChangeRequest{
			Topic:         topicName,
			Partition:     partitionName,
			ConsumerGroup: consumerGroup,
			ConsumerName:  consumerName,
			Offset:        offset,
		}

		var (
			res *sgproto.OffsetChangeReply
			err error
		)
		switch kind {
		case sgproto.MarkKind_Acknowledged:
			res, err = n.Acknowledge(ctx, change)
		case sgproto.MarkKind_NotAcknowledged:
			res, err = n.NotAcknowledge(ctx, change)
		case sgproto.MarkKind_Commited:
			res, err = n.Commit(ctx, change)
		case sgproto.MarkKind_Consumed:
			res, err = n.MarkConsumed(ctx, change)
		}
		if err != nil {
			return false, err
		}

		return res.Success, nil
	}

	state := &sgproto.MarkState{
		Kind: kind,
	}

	value, err := proto.Marshal(state)
	if err != nil {
		return false, err
	}

	res, err := b.Produce(ctx, &sgproto.ProduceMessageRequest{
		Topic:     ConsumerOffsetTopicName,
		Partition: p.Id,
		Messages: []*sgproto.Message{
			{
				Offset:        offset,
				Key:           partitionKey(topicName, partitionName, consumerGroup),
				ClusteringKey: generateClusterKey(offset, kind),
				Value:         value,
			},
		},
	})
	return res != nil, err
}

func (b *Broker) LastOffset(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, kind sgproto.MarkKind) (sandflake.ID, error) {
	topic := b.getTopic(ConsumerOffsetTopicName)
	pk := partitionKey(topicName, partitionName, consumerGroup)
	p := topic.ChoosePartitionForKey(pk)

	n := b.getPartitionLeader(ConsumerOffsetTopicName, p.Id)
	if n == nil {
		return sandflake.Nil, ErrNoLeaderFound
	}

	if n.Name != b.Name() {
		res, err := n.LastOffset(ctx, &sgproto.LastOffsetRequest{
			Topic:         topicName,
			Partition:     partitionName,
			ConsumerGroup: consumerGroup,
			ConsumerName:  consumerName,
			Kind:          kind,
		})
		if err != nil {
			return sandflake.Nil, err
		}

		return res.Offset, nil
	}

	lastKind := byte(kind)

	return b.last(p, pk, lastKind)
}

func (b *Broker) GetMarkStateMessage(ctx context.Context, topicName, partitionName, consumerGroup, consumerName string, offset sandflake.ID) (*sgproto.Message, error) {
	topic := b.getTopic(ConsumerOffsetTopicName)
	pk := partitionKey(topicName, partitionName, consumerGroup)
	p := topic.ChoosePartitionForKey(pk)

	n := b.getPartitionLeader(ConsumerOffsetTopicName, p.Id)
	if n == nil {
		return nil, ErrNoLeaderFound
	}

	if n.Name != b.Name() {
		res, err := n.GetMarkStateMessage(ctx, &sgproto.OffsetChangeRequest{
			Topic:         topicName,
			Partition:     partitionName,
			ConsumerGroup: consumerGroup,
			ConsumerName:  consumerName,
			Offset:        offset,
		})
		if err != nil {
			return nil, err
		}

		return res, nil
	}

	key := generatePrefixConsumerOffsetKey(pk, offset)

	msg, err := p.GetMessage(sandflake.Nil, key, nil)
	if err != nil {
		return nil, err
	}

	if msg == nil {
		return nil, status.Error(codes.NotFound, "mark state not found")
	}

	return msg, nil
}

func (b *Broker) last(p *topic.Partition, pk []byte, kind byte) (sandflake.ID, error) {
	msg, err := p.GetMessage(sandflake.Nil, pk, []byte{kind})
	if err != nil {
		return sandflake.Nil, err
	}

	if msg == nil {
		return sandflake.Nil, nil
	}

	if len(msg.Value) == 0 {
		return sandflake.Nil, fmt.Errorf("LastCommitedOffset malformed value '%v'", msg.Value)
	}

	return msg.Offset, nil
}

func (b *Broker) isAcknoweldged(ctx context.Context, topicName, partition, consumerGroup string, offset sandflake.ID) (bool, error) {
	topic := b.getTopic(ConsumerOffsetTopicName)
	if topic == nil {
		return false, ErrTopicNotFound
	}
	pk := partitionKey(topicName, partition, consumerGroup)
	p := topic.ChoosePartitionForKey(pk)
	clusterKey := generateClusterKey(offset, sgproto.MarkKind_Acknowledged)
	return b.hasKeyInPartition(ctx, ConsumerOffsetTopicName, p, pk, clusterKey)
}

func partitionKey(topicName, partitionName, consumerGroup string) []byte {
	return bytes.Join([][]byte{
		[]byte("offsets"),
		[]byte(topicName),
		[]byte(partitionName),
		[]byte(consumerGroup),
	}, storage.Separator)
}

func generateClusterKey(offset sandflake.ID, kind sgproto.MarkKind) []byte {
	return bytes.Join([][]byte{
		offset.Bytes(),
		[]byte{byte(kind)},
	}, storage.Separator)
}
