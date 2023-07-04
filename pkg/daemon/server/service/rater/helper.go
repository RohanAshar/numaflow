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

package server

import (
	"github.com/numaproj/numaflow/pkg/shared/logging"
	"go.uber.org/zap"
	"time"

	sharedqueue "github.com/numaproj/numaflow/pkg/shared/queue"
)

const IndexNotFound = -1

// UpdateCount updates the count of processed messages for a pod at a given time
func UpdateCount(q *sharedqueue.OverflowQueue[*TimestampedCounts], time int64, partitionReadCounts *PodReadCount) {
	items := q.Items()

	// find the element matching the input timestamp and update it
	for _, i := range items {
		if i.timestamp == time {
			i.Update(partitionReadCounts)
			return
		}
	}

	// if we cannot find a matching element, it means we need to add a new timestamped count to the queue
	tc := NewTimestampedCounts(time)
	tc.Update(partitionReadCounts)

	// close the window for the most recent timestamped partitionReadCounts
	switch n := len(items); n {
	case 0:
	// if the queue is empty, we just append the new timestamped count
	case 1:
		// if the queue has only one element, we close the window for this element
		items[0].CloseWindow(nil)
	default:
		// if the queue has more than one element, we close the window for the most recent element
		items[n-1].CloseWindow(items[n-2])
	}
	q.Append(tc)
}

// CalculateRate calculates the rate of the vertex partition in the last lookback seconds
func CalculateRate(q *sharedqueue.OverflowQueue[*TimestampedCounts], lookbackSeconds int64, partitionName, vertexName string) float64 {
	log := logging.NewLogger().Named("Helper")
	counts := q.Items()
	if len(counts) <= 1 {
		log.Info("Length of count < 1, rate 0", zap.String("Vertex", vertexName), zap.String("Partition", partitionName))
		return 0
	}
	startIndex := findStartIndex(lookbackSeconds, counts)
	endIndex := findEndIndex(counts)
	if startIndex == IndexNotFound || endIndex == IndexNotFound {
		if startIndex == IndexNotFound {
			log.Info("StartIdx not found, rate 0", zap.String("Vertex", vertexName), zap.String("Partition", partitionName))
		}
		if endIndex == IndexNotFound {
			log.Info("EndIdx not found, rate 0", zap.String("Vertex", vertexName), zap.String("Partition", partitionName))
		}
		return 0
	}

	delta := float64(0)
	// time diff in seconds.
	timeDiff := counts[endIndex].timestamp - counts[startIndex].timestamp
	if timeDiff == 0 {
		// if the time difference is 0, we return 0 to avoid division by 0
		// this should not happen in practice because we are using a 10s interval
		log.Info("Time diff is 0, rate 0", zap.String("Vertex", vertexName), zap.String("Partition", partitionName))
		return 0
	}
	// TODO: revisit this logic, we can just use the slope (counts[endIndex] - counts[startIndex] / timeDiff) to calculate the rate.
	for i := startIndex; i < endIndex; i++ {
		if counts[i+1] != nil && counts[i+1].IsWindowClosed() {
			delta += calculatePartitionDelta(counts[i+1], partitionName)
		}
	}
	if delta == 0.0 {
		log.Info("delta is 0, rate 0", zap.String("Vertex", vertexName), zap.String("Partition", partitionName))
	}
	return delta / float64(timeDiff)
}

// calculatePartitionDelta calculates the difference of the metric count between two timestamped counts for a given partition.
func calculatePartitionDelta(c1 *TimestampedCounts, partitionName string) float64 {
	tc1 := c1.PodDeltaCountSnapshot()
	delta := float64(0)
	for _, partitionCount := range tc1 {
		delta += partitionCount[partitionName]
	}
	return delta
}

// findStartIndex finds the index of the first element in the queue that is within the lookback seconds
// size of counts is at least 2
func findStartIndex(lookbackSeconds int64, counts []*TimestampedCounts) int {
	n := len(counts)
	now := time.Now().Truncate(time.Second * 10).Unix()
	if n < 2 || now-counts[n-2].timestamp > lookbackSeconds {
		// if the second last element is already outside the lookback window, we return IndexNotFound
		return IndexNotFound
	}

	startIndex := n - 2
	for i := n - 2; i >= 0; i-- {
		if now-counts[i].timestamp <= lookbackSeconds && counts[i].IsWindowClosed() {
			startIndex = i
		} else {
			break
		}
	}
	return startIndex
}

func findEndIndex(counts []*TimestampedCounts) int {
	for i := len(counts) - 1; i >= 0; i-- {
		// if a window is not closed, we exclude it from the rate calculation
		if counts[i].IsWindowClosed() {
			return i
		}
	}
	return IndexNotFound
}
