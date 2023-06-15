/*
Copyright 2022 The Numaproj Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package isbsvc

import (
	"context"

	"github.com/numaproj/numaflow/pkg/watermark/fetch"
)

// ISBService is an interface used to do the operations on ISBSvc
type ISBService interface {
	CreatePartitionsAndBuckets(ctx context.Context, partitions, buckets []string, opts ...CreateOption) error
	DeletePartitionsAndBuckets(ctx context.Context, partitions, buckets []string) error
	ValidatePartitionsAndBuckets(ctx context.Context, partitions, buckets []string) error
	GetPartitionInfo(ctx context.Context, partition string) (*PartitionInfo, error)
	CreateWatermarkFetcher(ctx context.Context, bucketName string, partitions int, isReduce bool) ([]fetch.Fetcher, error)
}

// createOptions describes the options for creating partitions and buckets
type createOptions struct {
	// config is configuration for the to be created partitions and buckets
	config string
}

type CreateOption func(*createOptions) error

// WithConfig sets partition and bucket config option
func WithConfig(conf string) CreateOption {
	return func(o *createOptions) error {
		o.config = conf
		return nil
	}
}

// PartitionInfo wraps the partition state information
type PartitionInfo struct {
	Name            string
	PendingCount    int64
	AckPendingCount int64
	TotalMessages   int64
}
