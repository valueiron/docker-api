package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	agentSetKey = "docker:agents"
	agentKeyFmt = "docker:agent:%s"
)

// Agent represents a registered remote Docker agent.
type Agent struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	TokenHash     string    `json:"tokenHash"`
	Status        string    `json:"status"`
	LastSeen      time.Time `json:"lastSeen"`
	DockerVersion string    `json:"dockerVersion"`
	Hostname      string    `json:"hostname"`
	CreatedAt     time.Time `json:"createdAt"`
}

// AgentStore persists agent registrations in Redis.
type AgentStore struct {
	client *redis.Client
}

// NewAgentStore connects to Redis at addr and returns a ready store.
func NewAgentStore(addr string) (*AgentStore, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}
	return &AgentStore{client: client}, nil
}

// Close shuts down the Redis connection.
func (s *AgentStore) Close() { _ = s.client.Close() }

// GenerateToken creates a cryptographically-random 32-byte token.
// Returns the plaintext token (shown to the user once) and its SHA-256 hash (stored).
func GenerateToken() (plaintext, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	plaintext = hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(sum[:])
	return
}

// HashToken returns the SHA-256 hex hash of token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateAgent inserts a new agent record and returns it.
func (s *AgentStore) CreateAgent(ctx context.Context, name, tokenHash string) (*Agent, error) {
	a := &Agent{
		ID:        uuid.New().String(),
		Name:      name,
		TokenHash: tokenHash,
		Status:    "disconnected",
		CreatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	pipe := s.client.Pipeline()
	pipe.Set(ctx, fmt.Sprintf(agentKeyFmt, a.ID), data, 0)
	pipe.SAdd(ctx, agentSetKey, a.ID)
	if _, err = pipe.Exec(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// GetAgent fetches an agent by ID. Returns (nil, nil) when not found.
func (s *AgentStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	data, err := s.client.Get(ctx, fmt.Sprintf(agentKeyFmt, id)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Agent
	if err = json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAgents returns all registered agents.
func (s *AgentStore) ListAgents(ctx context.Context) ([]*Agent, error) {
	ids, err := s.client.SMembers(ctx, agentSetKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*Agent, 0, len(ids))
	for _, id := range ids {
		a, err := s.GetAgent(ctx, id)
		if err != nil || a == nil {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// UpdateAgentStatus patches status, lastSeen, hostname, and dockerVersion.
func (s *AgentStore) UpdateAgentStatus(ctx context.Context, id, status, dockerVersion, hostname string) error {
	a, err := s.GetAgent(ctx, id)
	if err != nil || a == nil {
		return err
	}
	a.Status = status
	a.LastSeen = time.Now().UTC()
	if dockerVersion != "" {
		a.DockerVersion = dockerVersion
	}
	if hostname != "" {
		a.Hostname = hostname
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.client.Set(ctx, fmt.Sprintf(agentKeyFmt, id), data, 0).Err()
}

// DeleteAgent removes the agent record and its ID from the set.
func (s *AgentStore) DeleteAgent(ctx context.Context, id string) error {
	pipe := s.client.Pipeline()
	pipe.Del(ctx, fmt.Sprintf(agentKeyFmt, id))
	pipe.SRem(ctx, agentSetKey, id)
	_, err := pipe.Exec(ctx)
	return err
}

// FindByTokenHash returns the agent whose stored token hash matches hash, or nil.
func (s *AgentStore) FindByTokenHash(ctx context.Context, hash string) (*Agent, error) {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.TokenHash == hash {
			return a, nil
		}
	}
	return nil, nil
}
