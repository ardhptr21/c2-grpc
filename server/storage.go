package main

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	pb "github.com/ardhptr21/c2-grpc/pb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func connectMongo(ctx context.Context) (*mongo.Client, *mongo.Collection, *mongo.Collection, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI("mongodb://localhost:27017"))
	if err != nil {
		return nil, nil, nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	db := client.Database("c2_grpc")
	history := db.Collection("command_history")
	_, err = history.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "agent_id", Value: 1},
			{Key: "executed_at", Value: -1},
		},
	})
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	agents := db.Collection("agents")
	_, err = agents.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "agent_id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		_ = client.Disconnect(context.Background())
		return nil, nil, nil, err
	}

	return client, history, agents, nil
}

func trackTask(agentID string, task pb.Task) {
	taskRecordsMu.Lock()
	taskRecords[task.GetTaskId()] = TaskRecord{
		TaskID:     task.GetTaskId(),
		AgentID:    agentID,
		Command:    task.GetCommand(),
		Args:       task.GetArgs(),
		ExecutedAt: time.Now(),
	}
	taskRecordsMu.Unlock()
}

func upsertAgentRecord(agentID string, agent *Agent, isOnline bool) {
	if agentCollection == nil || agent == nil {
		return
	}

	doc := bson.M{
		"agent_id":   agentID,
		"hostname":   agent.Hostname,
		"os":         agent.OS,
		"ip":         agent.IP,
		"status":     agent.Status,
		"last_seen":  agent.LastSeen,
		"is_online":  isOnline,
		"updated_at": time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := agentCollection.UpdateOne(
		ctx,
		bson.M{"agent_id": agentID},
		bson.M{"$set": doc},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		log.Printf("[server] agent upsert error for %s: %v", agentID, err)
	}
}

func markAgentOffline(agentID string, hostname string) {
	if agentCollection == nil || agentID == "" {
		return
	}

	update := bson.M{
		"status":     "OFFLINE",
		"is_online":  false,
		"updated_at": time.Now(),
	}
	if hostname != "" {
		update["hostname"] = hostname
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := agentCollection.UpdateOne(
		ctx,
		bson.M{"agent_id": agentID},
		bson.M{"$set": update},
	)
	if err != nil {
		log.Printf("[server] mark agent offline error for %s: %v", agentID, err)
	}
}

func loadKnownAgents(ctx context.Context) ([]AgentRecord, error) {
	if agentCollection == nil {
		return nil, nil
	}

	cursor, err := agentCollection.Find(
		ctx,
		bson.M{},
		options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}}),
	)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var docs []AgentRecord
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func persistTaskHistory(taskID string) {
	taskRecordsMu.Lock()
	record, ok := taskRecords[taskID]
	if ok {
		delete(taskRecords, taskID)
	}
	taskRecordsMu.Unlock()
	if !ok {
		return
	}

	outputsMu.Lock()
	lines := append([]string(nil), outputs[taskID]...)
	delete(outputs, taskID)
	outputsMu.Unlock()

	if historyCollection == nil {
		return
	}

	doc := TaskExecution{
		TaskID:      record.TaskID,
		AgentID:     record.AgentID,
		Command:     record.Command,
		Args:        record.Args,
		Output:      strings.Join(lines, ""),
		ExecutedAt:  record.ExecutedAt,
		CompletedAt: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := historyCollection.InsertOne(ctx, doc); err != nil {
		log.Printf("[server] mongo insert error for task %s: %v", taskID, err)
		return
	}

	entryPayload, err := json.Marshal(&pb.AgentHistoryEntry{
		TaskId:      doc.TaskID,
		AgentId:     doc.AgentID,
		Command:     doc.Command,
		Args:        doc.Args,
		Output:      doc.Output,
		ExecutedAt:  doc.ExecutedAt.UnixMilli(),
		CompletedAt: doc.CompletedAt.UnixMilli(),
	})
	if err != nil {
		log.Printf("[server] history payload encode error for task %s: %v", taskID, err)
		return
	}

	broadcastToOperators(&pb.OperatorEvent{
		Type:    "history_updated",
		AgentId: record.AgentID,
		Payload: string(entryPayload),
	})
}
