// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package agentcfg // import "github.com/elastic/opentelemetry-collector-components/internal/agentcfg"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
)

const ElasticsearchIndexName = ".apm-agent-configuration"

const (
	// ErrInfrastructureNotReady is returned when a fetch request comes in while
	// the infrastructure is not ready to serve the request.
	// This may happen when the local cache is not initialized and no fallback fetcher is configured.
	ErrInfrastructureNotReady = "agentcfg infrastructure is not ready"

	// ErrNoValidElasticsearchConfig is an error where the server is
	// not properly configured to fetch agent configuration.
	ErrNoValidElasticsearchConfig = "no valid elasticsearch config to fetch agent config"
)

const (
	refreshCacheTimeout = 5 * time.Second
	loggerRateLimit     = time.Minute
)

// TODO:
// - Add Otel tracer
// - Collection metrics
type ElasticsearchFetcher struct {
	last             time.Time
	client           *elasticsearch.Client
	logger           *zap.Logger
	cache            []AgentConfig
	cacheDuration    time.Duration
	searchSize       int
	mu               sync.RWMutex
	invalidESCfg     atomic.Bool
	cacheInitialized atomic.Bool
}

func NewElasticsearchFetcher(
	client *elasticsearch.Client,
	cacheDuration time.Duration,
	logger *zap.Logger,
) *ElasticsearchFetcher {
	return &ElasticsearchFetcher{
		client:        client,
		cacheDuration: cacheDuration,
		searchSize:    100,
		logger:        logger,
	}
}

// Fetch finds a matching agent config based on the received query.
func (f *ElasticsearchFetcher) Fetch(ctx context.Context, query Query) (Result, error) {
	if f.cacheInitialized.Load() {
		// Happy path: serve fetch requests using an initialized cache.
		f.mu.RLock()
		defer f.mu.RUnlock()
		return matchAgentConfig(query, f.cache), nil
	}

	if f.invalidESCfg.Load() {
		return Result{}, errors.New(ErrNoValidElasticsearchConfig)
	}

	return Result{}, errors.New(ErrInfrastructureNotReady)
}

// Run refreshes the fetcher cache by querying Elasticsearch periodically.
func (f *ElasticsearchFetcher) Run(ctx context.Context) error {
	refresh := func() bool {
		// refresh returns a bool that indicates whether Run should return
		// immediately without error, e.g. due to invalid Elasticsearch config.
		if err := f.refreshCache(ctx); err != nil {

			f.logger.Error(fmt.Sprintf("refresh cache error: %s", err))
			if f.invalidESCfg.Load() {
				f.logger.Warn("stopping refresh cache background job: elasticsearch config is invalid")
				return true
			}
		} else {
			f.logger.Debug("refresh cache success")
		}
		return false
	}

	// Trigger initial run.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		if stop := refresh(); stop {
			return nil
		}
	}

	// Then schedule subsequent runs.
	t := time.NewTicker(f.cacheDuration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if stop := refresh(); stop {
				return nil
			}
		}
	}
}

type cacheResult struct {
	ScrollID string `json:"_scroll_id"`
	Hits     struct {
		Hits []struct {
			Source struct {
				Settings map[string]string `json:"settings"`
				Service  struct {
					Name        string `json:"name"`
					Environment string `json:"environment"`
				} `json:"service"`
				AgentName string `json:"agent_name"`
				ETag      string `json:"etag"`
			} `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func (f *ElasticsearchFetcher) refreshCache(ctx context.Context) (err error) {
	scrollID := ""
	buffer := make([]AgentConfig, 0, len(f.cache))

	// The refresh cache operation should complete within refreshCacheTimeout.
	ctx, cancel := context.WithTimeout(ctx, refreshCacheTimeout)
	defer cancel()

	for {
		result, err := f.singlePageRefresh(ctx, scrollID)
		if err != nil {
			f.clearScroll(ctx, scrollID)
			return err
		}

		for _, hit := range result.Hits.Hits {
			buffer = append(buffer, AgentConfig{
				ServiceName:        hit.Source.Service.Name,
				ServiceEnvironment: hit.Source.Service.Environment,
				AgentName:          hit.Source.AgentName,
				Etag:               hit.Source.ETag,
				Config:             hit.Source.Settings,
			})
		}
		scrollID = result.ScrollID
		if len(result.Hits.Hits) == 0 {
			break
		}
	}

	f.clearScroll(ctx, scrollID)

	f.mu.Lock()
	f.cache = buffer
	f.mu.Unlock()
	f.cacheInitialized.Store(true)
	f.last = time.Now()
	return nil
}

func (f *ElasticsearchFetcher) clearScroll(ctx context.Context, scrollID string) {
	resp, err := esapi.ClearScrollRequest{
		ScrollID: []string{scrollID},
	}.Do(ctx, f.client)
	if err != nil {
		f.logger.Warn(fmt.Sprintf("failed to clear scroll: %v", err))
		return
	}

	if resp.IsError() {
		f.logger.Warn(fmt.Sprintf("clearscroll request returned error: %s", resp.Status()))
	}

	resp.Body.Close()
}

func (f *ElasticsearchFetcher) singlePageRefresh(ctx context.Context, scrollID string) (cacheResult, error) {
	var result cacheResult
	var err error
	var resp *esapi.Response

	switch scrollID {
	case "":
		resp, err = esapi.SearchRequest{
			Index:  []string{ElasticsearchIndexName},
			Size:   &f.searchSize,
			Scroll: f.cacheDuration,
		}.Do(ctx, f.client)
	default:
		resp, err = esapi.ScrollRequest{
			ScrollID: scrollID,
			Scroll:   f.cacheDuration,
		}.Do(ctx, f.client)
	}
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		// Elasticsearch returns 401 on unauthorized requests and 403 on insufficient permission
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			f.invalidESCfg.Store(true)
		}
		bodyBytes, err := io.ReadAll(resp.Body)
		if err == nil {
			f.logger.Debug(fmt.Sprintf("refresh cache elasticsearch returned status %d: %s", resp.StatusCode, string(bodyBytes)))
		}
		return result, fmt.Errorf("refresh cache elasticsearch returned status %d", resp.StatusCode)
	}
	return result, json.NewDecoder(resp.Body).Decode(&result)
}
