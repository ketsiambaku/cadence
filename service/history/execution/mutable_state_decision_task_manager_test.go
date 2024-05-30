// The MIT License (MIT)
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package execution

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log/loggerimpl"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/log/testlogger"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/common/types/testdata"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/constants"
	"github.com/uber/cadence/service/history/shard"
)

func TestReplicateDecisionTaskCompletedEvent(t *testing.T) {
	mockShard := shard.NewTestContext(
		t,
		gomock.NewController(t),
		&persistence.ShardInfo{
			ShardID:          0,
			RangeID:          1,
			TransferAckLevel: 0,
		},
		config.NewForTest(),
	)
	mockShard.GetConfig().MutableStateChecksumGenProbability = func(domain string) int { return 100 }
	mockShard.GetConfig().MutableStateChecksumVerifyProbability = func(domain string) int { return 100 }
	logger := mockShard.GetLogger()
	mockShard.Resource.DomainCache.EXPECT().GetDomainID(constants.TestDomainName).Return(constants.TestDomainID, nil).AnyTimes()

	m := &mutableStateDecisionTaskManagerImpl{
		msb: newMutableStateBuilder(mockShard, logger, constants.TestLocalDomainEntry),
	}
	eventType := types.EventTypeActivityTaskCompleted
	e := &types.HistoryEvent{
		ID:        1,
		EventType: &eventType,
	}
	err := m.ReplicateDecisionTaskCompletedEvent(e)
	assert.NoError(t, err)

	// test when domainEntry is missed
	m.msb.domainEntry = nil
	err = m.ReplicateDecisionTaskCompletedEvent(e)
	assert.NoError(t, err)

	// test when config is nil
	m.msb = newMutableStateBuilder(mockShard, logger, constants.TestLocalDomainEntry)
	m.msb.config = nil
	err = m.ReplicateDecisionTaskCompletedEvent(e)
	assert.NoError(t, err)
}

func TestReplicateDecisionTaskScheduledEvent(t *testing.T) {
	version := int64(123)
	scheduleID := int64(1)
	taskList := "task-list"
	startToCloseTimeoutSeconds := int32(100)
	attempt := int64(1)
	scheduleTimestamp := int64(1)
	originalScheduledTimestamp := int64(0)
	bypassTaskGeneration := false
	tests := []struct {
		name         string
		assertions   func(t *testing.T, info *DecisionInfo, err error, logs *observer.ObservedLogs)
		expectations func(mgr *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "success",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						State: 0, // persistence.WorkflowStateCreated
					},
					taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
				}
			},
			expectations: func(mgr *mutableStateDecisionTaskManagerImpl) {
				mgr.msb.taskGenerator.(*MockMutableStateTaskGenerator).EXPECT().GenerateDecisionScheduleTasks(scheduleID)
			},
			assertions: func(t *testing.T, info *DecisionInfo, err error, observedLogs *observer.ObservedLogs) {
				require.NoError(t, err)
				assert.Equal(t, version, info.Version)
				assert.Equal(t, scheduleID, info.ScheduleID)
				assert.Equal(t, taskList, info.TaskList)
				assert.Equal(t, attempt, info.Attempt)
				assert.Equal(t, scheduleTimestamp, info.ScheduledTimestamp)
				assert.Equal(t, originalScheduledTimestamp, info.OriginalScheduledTimestamp)
				assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
					"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
					scheduleID,
					common.EmptyEventID,
					common.EmptyUUID,
					startToCloseTimeoutSeconds,
					attempt,
					0,
				)).Len())
			},
		},
		{
			name: "UpdateWorkflowStateCloseStatus failure",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						State: 2, // persistence.WorkflowStateCompleted
					},
					taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
				}
			},
			assertions: func(t *testing.T, info *DecisionInfo, err error, observedLogs *observer.ObservedLogs) {
				require.Error(t, err)
				assert.Equal(t, "unable to change workflow state from 2 to 1, close status 0", err.Error())
				assert.Nil(t, info)
			},
		},
		{
			name: "GenerateDecisionScheduleTasks failure",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						State: 0, // persistence.WorkflowStateCreated
					},
					taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
				}
			},
			expectations: func(mgr *mutableStateDecisionTaskManagerImpl) {
				mgr.msb.taskGenerator.(*MockMutableStateTaskGenerator).EXPECT().GenerateDecisionScheduleTasks(scheduleID).Return(errors.New("some error"))
			},
			assertions: func(t *testing.T, info *DecisionInfo, err error, observedLogs *observer.ObservedLogs) {
				require.Error(t, err)
				assert.Equal(t, "some error", err.Error())
				assert.Nil(t, info)
				assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
					"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
					scheduleID,
					common.EmptyEventID,
					common.EmptyUUID,
					startToCloseTimeoutSeconds,
					attempt,
					0,
				)).Len())
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			core, observedLogs := observer.New(zap.DebugLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			info, err := m.ReplicateDecisionTaskScheduledEvent(version, scheduleID, taskList, startToCloseTimeoutSeconds, attempt, scheduleTimestamp, originalScheduledTimestamp, bypassTaskGeneration)
			test.assertions(t, info, err, observedLogs)
		})
	}
}

func TestReplicateTransientDecisionTaskScheduled(t *testing.T) {
	tests := []struct {
		name         string
		expectations func(mgr *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
		assertions   func(t *testing.T, err error, logs *observer.ObservedLogs)
	}{
		{
			name: "success - decision updated",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: common.EmptyEventID,
						DecisionAttempt:    1,
					},
					taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
					timeSource:    clock.NewMockedTimeSource(),
				}
			},
			expectations: func(mgr *mutableStateDecisionTaskManagerImpl) {
				mgr.msb.taskGenerator.(*MockMutableStateTaskGenerator).EXPECT().GenerateDecisionScheduleTasks(int64(0))
			},
			assertions: func(t *testing.T, err error, observedLogs *observer.ObservedLogs) {
				require.NoError(t, err)
				assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
					"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
					0, common.EmptyEventID, common.EmptyUUID, 0, 1, 0)).Len())
			},
		},
		{
			name: "success - decision need no update",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: 0, // pending decisions
						DecisionAttempt:    0,
					},
					taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
					timeSource:    clock.NewMockedTimeSource(),
				}
			},
			assertions: func(t *testing.T, err error, observedLogs *observer.ObservedLogs) {
				require.NoError(t, err)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			core, observedLogs := observer.New(zap.DebugLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			err := m.ReplicateTransientDecisionTaskScheduled()
			test.assertions(t, err, observedLogs)
		})
	}
}

func TestCreateTransientDecisionEvents(t *testing.T) {
	m := &mutableStateDecisionTaskManagerImpl{
		msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{TaskList: "some-task-list"}},
	}
	decision := &DecisionInfo{
		ScheduleID:                 0,
		StartedID:                  1,
		RequestID:                  "some-requestID",
		DecisionTimeout:            100,
		Attempt:                    1,
		ScheduledTimestamp:         10,
		StartedTimestamp:           10,
		OriginalScheduledTimestamp: 10,
	}
	scheduledEvent, startedEvent := m.CreateTransientDecisionEvents(decision, "identity")
	require.NotNil(t, scheduledEvent)
	assert.Equal(t, decision.ScheduleID, scheduledEvent.ID)
	assert.Equal(t, &decision.ScheduledTimestamp, scheduledEvent.Timestamp)
	assert.Equal(t, m.msb.executionInfo.TaskList, scheduledEvent.DecisionTaskScheduledEventAttributes.TaskList.Name)
	assert.Equal(t, &decision.DecisionTimeout, scheduledEvent.DecisionTaskScheduledEventAttributes.StartToCloseTimeoutSeconds)
	assert.Equal(t, decision.Attempt, scheduledEvent.DecisionTaskScheduledEventAttributes.Attempt)

	require.NotNil(t, startedEvent)
	assert.Equal(t, decision.StartedID, startedEvent.ID)
	assert.Equal(t, &decision.StartedTimestamp, startedEvent.Timestamp)
	assert.Equal(t, decision.ScheduleID, startedEvent.DecisionTaskStartedEventAttributes.ScheduledEventID)
	assert.Equal(t, decision.RequestID, startedEvent.DecisionTaskStartedEventAttributes.RequestID)
	assert.Equal(t, "identity", startedEvent.DecisionTaskStartedEventAttributes.Identity)
}

func TestGetDecisionScheduleToStartTimeout(t *testing.T) {
	t.Run("sticky taskList", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				StickyTaskList:               "some-sticky-task-list",
				StickyScheduleToStartTimeout: 100,
			}},
		}
		duration := m.GetDecisionScheduleToStartTimeout()
		assert.Equal(t, time.Duration(m.msb.executionInfo.StickyScheduleToStartTimeout)*time.Second, duration)
	})
}

func TestHasProcessedOrPendingDecision(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				DecisionScheduleID: 0, // has pending decisions
			}},
		}
		ok := m.HasProcessedOrPendingDecision()
		assert.True(t, ok)
	})

	t.Run("false", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				// has no pending decisions
				DecisionScheduleID: common.EmptyEventID,
				LastProcessedEvent: common.EmptyEventID,
			}},
		}
		ok := m.HasProcessedOrPendingDecision()
		assert.False(t, ok)
	})
}

func TestGetInFlightDecision(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				DecisionScheduleID: 0,
				DecisionStartedID:  1,
				StickyTaskList:     "some-sticky-task-list",
			}},
		}
		decision, ok := m.GetInFlightDecision()
		require.NotNil(t, decision)
		assert.True(t, ok)

		assert.Equal(t, m.msb.executionInfo.DecisionVersion, decision.Version)
		assert.Equal(t, m.msb.executionInfo.DecisionScheduleID, decision.ScheduleID)
		assert.Equal(t, m.msb.executionInfo.DecisionStartedID, decision.StartedID)
		assert.Equal(t, m.msb.executionInfo.DecisionRequestID, decision.RequestID)
		assert.Equal(t, int64(m.msb.executionInfo.Attempt), decision.Attempt)
		assert.Equal(t, m.msb.executionInfo.DecisionStartedTimestamp, decision.StartedTimestamp)
		assert.Equal(t, m.msb.executionInfo.DecisionScheduledTimestamp, decision.ScheduledTimestamp)
		assert.Equal(t, m.msb.executionInfo.StickyTaskList, decision.TaskList)
	})

	t.Run("failure", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				DecisionScheduleID: common.EmptyEventID,
				DecisionStartedID:  common.EmptyEventID,
			}},
		}
		decision, value := m.GetInFlightDecision()
		require.Nil(t, decision)
		assert.False(t, value)
	})
}

func TestGetPendingDecision(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				DecisionScheduleID: 0,
				DecisionStartedID:  1,
				StickyTaskList:     "some-sticky-task-list",
			}},
		}
		decision, ok := m.GetPendingDecision()
		require.NotNil(t, decision)
		assert.True(t, ok)

		assert.Equal(t, m.msb.executionInfo.DecisionVersion, decision.Version)
		assert.Equal(t, m.msb.executionInfo.DecisionScheduleID, decision.ScheduleID)
		assert.Equal(t, m.msb.executionInfo.DecisionStartedID, decision.StartedID)
		assert.Equal(t, m.msb.executionInfo.DecisionRequestID, decision.RequestID)
		assert.Equal(t, int64(m.msb.executionInfo.Attempt), decision.Attempt)
		assert.Equal(t, m.msb.executionInfo.DecisionStartedTimestamp, decision.StartedTimestamp)
		assert.Equal(t, m.msb.executionInfo.DecisionScheduledTimestamp, decision.ScheduledTimestamp)
		assert.Equal(t, m.msb.executionInfo.StickyTaskList, decision.TaskList)
	})

	t.Run("failure", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{executionInfo: &persistence.WorkflowExecutionInfo{
				DecisionScheduleID: common.EmptyEventID,
			}},
		}
		decision, value := m.GetPendingDecision()
		require.Nil(t, decision)
		assert.False(t, value)
	})
}

func TestReplicateDecisionTaskStartedEvent(t *testing.T) {
	var version int64 = 123
	var scheduleID int64 = 1
	var startedID int64 = 2
	requestID := "some-request-id"
	var timeStamp int64 = 1
	var originalTimeStamp int64 = 1

	t.Run("success", func(t *testing.T) {
		core, observedLogs := observer.New(zap.DebugLevel)
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{
				executionInfo: &persistence.WorkflowExecutionInfo{
					DecisionScheduleID:                 scheduleID,
					DecisionVersion:                    version,
					DecisionStartedID:                  startedID,
					DecisionRequestID:                  requestID,
					DecisionStartedTimestamp:           timeStamp,
					DecisionOriginalScheduledTimestamp: originalTimeStamp,
					TaskList:                           "some-taskList",
				},
				taskGenerator: NewMockMutableStateTaskGenerator(gomock.NewController(t)),
				timeSource:    clock.NewMockedTimeSource(),
				logger:        loggerimpl.NewLogger(zap.New(core)),
			},
		}
		var decision *DecisionInfo
		m.msb.taskGenerator.(*MockMutableStateTaskGenerator).EXPECT().GenerateDecisionStartTasks(scheduleID)
		result, err := m.ReplicateDecisionTaskStartedEvent(decision, version, scheduleID, startedID, requestID, timeStamp)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
			"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
			scheduleID, startedID, requestID, 0, m.msb.executionInfo.Attempt, timeStamp)).Len())
		assert.Equal(t, version, result.Version)
		assert.Equal(t, scheduleID, result.ScheduleID)
		assert.Equal(t, startedID, result.StartedID)
		assert.Equal(t, int64(m.msb.executionInfo.Attempt), result.Attempt)
		assert.Equal(t, requestID, result.RequestID)
		assert.Equal(t, m.msb.executionInfo.TaskList, result.TaskList)
		assert.Equal(t, timeStamp, result.StartedTimestamp)
		assert.Equal(t, m.msb.executionInfo.DecisionOriginalScheduledTimestamp, result.OriginalScheduledTimestamp)
	})

	t.Run("failure", func(t *testing.T) {
		m := &mutableStateDecisionTaskManagerImpl{
			msb: &mutableStateBuilder{
				executionInfo: &persistence.WorkflowExecutionInfo{
					DecisionScheduleID: common.EmptyEventID,
					DecisionAttempt:    1,
				},
			},
		}
		var decision *DecisionInfo
		result, err := m.ReplicateDecisionTaskStartedEvent(decision, version, scheduleID, startedID, requestID, timeStamp)
		require.Error(t, err)
		require.Nil(t, result)
		assert.Equal(t, fmt.Sprintf("unable to find decision: %v", scheduleID), err.Error())

	})
}

func TestReplicateDecisionTaskTimedOutEvent(t *testing.T) {
	core, observedLogs := observer.New(zap.DebugLevel)
	m := &mutableStateDecisionTaskManagerImpl{
		msb: &mutableStateBuilder{
			executionInfo: &persistence.WorkflowExecutionInfo{
				TaskList:       "some-taskList",
				StickyTaskList: "some-sticky-taskList",
			},
			logger: loggerimpl.NewLogger(zap.New(core)),
		},
	}
	event := &types.HistoryEvent{
		DecisionTaskTimedOutEventAttributes: &types.DecisionTaskTimedOutEventAttributes{
			TimeoutType: func(i int32) *types.TimeoutType {
				timeout := new(types.TimeoutType)
				*timeout = types.TimeoutType(i)
				return timeout
			}(1), // types.TimeoutTypeScheduleToStart
			Cause: func(i int32) *types.DecisionTaskTimedOutCause {
				cause := new(types.DecisionTaskTimedOutCause)
				*cause = types.DecisionTaskTimedOutCause(i) // types.DecisionTaskTimedOutCauseReset
				return cause
			}(1),
		},
	}
	err := m.ReplicateDecisionTaskTimedOutEvent(event)
	require.NoError(t, err)
	assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
		"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
		m.msb.executionInfo.DecisionScheduleID,
		m.msb.executionInfo.DecisionStartedID,
		m.msb.executionInfo.DecisionRequestID,
		m.msb.executionInfo.DecisionTimeout,
		m.msb.executionInfo.DecisionAttempt,
		m.msb.executionInfo.DecisionStartedTimestamp,
	)).Len())
}

func TestAddDecisionTaskScheduleToStartTimeoutEvent(t *testing.T) {
	var scheduledID int64 = 1
	tests := []struct {
		name         string
		assertions   func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl)
		expectations func(m *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "success",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: scheduledID,
						DecisionAttempt:    int64(dynamicconfig.DecisionRetryCriticalAttempts),
					},
					timeSource:    clock.NewMockedTimeSource(),
					domainEntry:   constants.TestLocalDomainEntry,
					metricsClient: metrics.NewNoopMetricsClient(),
				}
			},
			expectations: func(m *mutableStateDecisionTaskManagerImpl) {
				m.msb.shard.(*shard.MockContext).EXPECT().GetConfig().Return(config.NewForTest())
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				assert.Equal(t, 1, observedLogs.FilterMessage("Critical error processing decision task, retrying.").Len())
				assert.Equal(t, 1, observedLogs.FilterMessage(fmt.Sprintf(
					"Decision Updated: {Schedule: %v, Started: %v, ID: %v, Timeout: %v, Attempt: %v, Timestamp: %v}",
					m.msb.executionInfo.DecisionScheduleID,
					m.msb.executionInfo.DecisionStartedID,
					m.msb.executionInfo.DecisionRequestID,
					m.msb.executionInfo.DecisionTimeout,
					m.msb.executionInfo.DecisionAttempt,
					m.msb.executionInfo.DecisionStartedTimestamp,
				)).Len())
			},
		},
		{
			name: "failure",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionStartedID: 1,
					},
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.Error(t, err)
				assert.Equal(t, m.msb.createInternalServerError(tag.WorkflowActionDecisionTaskTimedOut), err)
				assert.Nil(t, event)
				assert.Equal(t, 1, logs.FilterMessage(mutableStateInvalidHistoryActionMsg).Len())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			m.msb.hBuilder = NewHistoryBuilder(m.msb)
			m.msb.shard = shard.NewMockContext(gomock.NewController(t))
			core, _ := observer.New(zap.WarnLevel)
			core, observedLogs := observer.New(zap.DebugLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			event, err := m.AddDecisionTaskScheduleToStartTimeoutEvent(scheduledID)
			test.assertions(t, event, err, observedLogs, m)
		})
	}
}

func TestAddDecisionTaskResetTimeoutEvent(t *testing.T) {
	var scheduledID int64 = 1
	var baseRunID string = testdata.RunID
	var newRunID string = constants.TestRunID
	var forkEventVersion int64 = 123
	var reason string = "some-reason"
	var resetRequestID string = testdata.RequestID

	tests := []struct {
		name         string
		assertions   func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl)
		expectations func(m *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "success",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: scheduledID,
						DecisionAttempt:    int64(dynamicconfig.DecisionRetryCriticalAttempts),
					},
					timeSource:       clock.NewMockedTimeSource(),
					domainEntry:      constants.TestLocalDomainEntry,
					metricsClient:    metrics.NewNoopMetricsClient(),
					config:           config.NewForTest(),
					workflowRequests: make(map[persistence.WorkflowRequest]struct{}),
				}
			},
			expectations: func(m *mutableStateDecisionTaskManagerImpl) {
				m.msb.shard.(*shard.MockContext).EXPECT().GetConfig().Return(config.NewForTest())
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.NoError(t, err)
				require.NotNil(t, event)
				assert.Equal(t, 1, observedLogs.FilterMessage("Critical error processing decision task, retrying.").Len())
				assert.Equal(t, scheduledID, event.DecisionTaskTimedOutEventAttributes.ScheduledEventID)
				assert.Equal(t, baseRunID, event.DecisionTaskTimedOutEventAttributes.BaseRunID)
				assert.Equal(t, newRunID, event.DecisionTaskTimedOutEventAttributes.NewRunID)
				assert.Equal(t, reason, event.DecisionTaskTimedOutEventAttributes.Reason)
				assert.Equal(t, resetRequestID, event.DecisionTaskTimedOutEventAttributes.RequestID)
				assert.Equal(t, forkEventVersion, event.DecisionTaskTimedOutEventAttributes.ForkEventVersion)

			},
		},
		{
			name: "internal server error",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: 0,
					},
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.Error(t, err)
				assert.Equal(t, m.msb.createInternalServerError(tag.WorkflowActionDecisionTaskTimedOut), err)
				assert.Nil(t, event)
				assert.Equal(t, 1, logs.FilterMessage(mutableStateInvalidHistoryActionMsg).Len())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			m.msb.hBuilder = NewHistoryBuilder(m.msb)
			m.msb.shard = shard.NewMockContext(gomock.NewController(t))
			core, _ := observer.New(zap.WarnLevel)
			core, observedLogs := observer.New(zap.DebugLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			event, err := m.AddDecisionTaskResetTimeoutEvent(scheduledID, baseRunID, newRunID, forkEventVersion, reason, resetRequestID)
			test.assertions(t, event, err, observedLogs, m)
		})
	}
}

func TestAddFirstDecisionTaskScheduled(t *testing.T) {
	tests := []struct {
		name         string
		assertions   func(t *testing.T, err error)
		expectations func(m *mutableStateDecisionTaskManagerImpl, event *types.HistoryEvent)
		newMsb       func(t *testing.T) *mutableStateBuilder
		event        *types.HistoryEvent
	}{
		{
			name: "AddDecisionTaskScheduledEvent failure",
			event: &types.HistoryEvent{
				WorkflowExecutionStartedEventAttributes: &types.WorkflowExecutionStartedEventAttributes{},
			},
			expectations: func(m *mutableStateDecisionTaskManagerImpl, event *types.HistoryEvent) {
				m.msb.shard.(*shard.MockContext).EXPECT().GenerateTransferTaskIDs(2).Return(nil, errors.New("some error"))
			},
			assertions: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Equal(t, "some error", err.Error())
			},
		},
		{
			name: "GenerateDelayedDecisionTasks failure",
			event: &types.HistoryEvent{
				WorkflowExecutionStartedEventAttributes: &types.WorkflowExecutionStartedEventAttributes{FirstDecisionTaskBackoffSeconds: func(i int32) *int32 { return &i }(100)},
			},
			expectations: func(m *mutableStateDecisionTaskManagerImpl, event *types.HistoryEvent) {
				m.msb.taskGenerator.(*MockMutableStateTaskGenerator).EXPECT().GenerateDelayedDecisionTasks(event).Return(errors.New("some error"))
			},
			assertions: func(t *testing.T, err error) {
				require.Error(t, err)
				assert.Equal(t, "some error", err.Error())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			m := &mutableStateDecisionTaskManagerImpl{
				msb: &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: common.EmptyEventID,
						StickyTaskList:     "some-sticky-tasklist",
						DecisionStartedID:  1,
					},
					domainEntry:   constants.TestLocalDomainEntry,
					shard:         shard.NewMockContext(ctrl),
					taskGenerator: NewMockMutableStateTaskGenerator(ctrl),
					timeSource:    clock.NewRealTimeSource(),
					config:        config.NewForTest(),
					logger:        testlogger.New(t),
				},
			}
			m.msb.executionInfo.LastUpdatedTimestamp = m.msb.timeSource.Now()
			m.msb.hBuilder = NewHistoryBuilder(m.msb)
			m.msb.hBuilder.history = append(m.msb.hBuilder.history, &types.HistoryEvent{ID: common.BufferedEventID})
			m.msb.hBuilder.transientHistory = append(m.msb.hBuilder.history, &types.HistoryEvent{})
			m.msb.decisionTaskManager = m
			if test.expectations != nil {
				test.expectations(m, test.event)
			}
			err := m.AddFirstDecisionTaskScheduled(test.event)
			test.assertions(t, err)
		})
	}
}

func TestAddDecisionTaskTimedOutEvent(t *testing.T) {
	var scheduledID int64 = 1
	var startedID int64 = 2
	tests := []struct {
		name       string
		assertions func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl)
		newMsb     func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "failure",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: 0, // must be != scheduledID
					},
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, logs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.Error(t, err)
				assert.Equal(t, m.msb.createInternalServerError(tag.WorkflowActionDecisionTaskTimedOut), err)
				assert.Nil(t, event)
				assert.Equal(t, 1, logs.FilterMessage(mutableStateInvalidHistoryActionMsg).Len())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			core, observedLogs := observer.New(zap.WarnLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			event, err := m.AddDecisionTaskTimedOutEvent(scheduledID, startedID)
			test.assertions(t, event, err, observedLogs, m)
		})
	}
}

func TestAddDecisionTaskFailedEvent(t *testing.T) {
	var scheduledID int64 = 1
	var startedID int64 = 1
	var forkEventVersion int64 = 123
	baseRunID := testdata.RunID
	newRunID := constants.TestRunID
	resetRequestID := testdata.RequestID
	cause := types.DecisionTaskFailedCause(0) // any
	details := []byte("some-details")
	identity := testdata.Identity
	binChecksum := testdata.Checksum
	reason := testdata.Reason

	tests := []struct {
		name         string
		assertions   func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl)
		expectations func(m *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "success",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: scheduledID,
						DecisionStartedID:  startedID,
					},
					timeSource:       clock.NewMockedTimeSource(),
					domainEntry:      constants.TestLocalDomainEntry,
					metricsClient:    metrics.NewNoopMetricsClient(),
					config:           config.NewForTest(),
					workflowRequests: make(map[persistence.WorkflowRequest]struct{}),
				}
			},
			expectations: func(m *mutableStateDecisionTaskManagerImpl) {
				m.msb.shard.(*shard.MockContext).EXPECT().GetConfig().Return(config.NewForTest())
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.NoError(t, err)
				require.NotNil(t, event)
				assert.Equal(t, scheduledID, event.DecisionTaskFailedEventAttributes.ScheduledEventID)
				assert.Equal(t, baseRunID, event.DecisionTaskFailedEventAttributes.BaseRunID)
				assert.Equal(t, newRunID, event.DecisionTaskFailedEventAttributes.NewRunID)
				assert.Equal(t, common.StringPtr(reason), event.DecisionTaskFailedEventAttributes.Reason)
				assert.Equal(t, resetRequestID, event.DecisionTaskFailedEventAttributes.RequestID)
				assert.Equal(t, forkEventVersion, event.DecisionTaskFailedEventAttributes.ForkEventVersion)

			},
		},
		{
			name: "internal server error",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: 0, // must be != scheduledID
					},
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.Error(t, err)
				assert.Equal(t, m.msb.createInternalServerError(tag.WorkflowActionDecisionTaskFailed), err)
				assert.Nil(t, event)
				assert.Equal(t, 1, observedLogs.FilterMessage(mutableStateInvalidHistoryActionMsg).Len())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			m.msb.hBuilder = NewHistoryBuilder(m.msb)
			m.msb.shard = shard.NewMockContext(gomock.NewController(t))
			core, observedLogs := observer.New(zap.WarnLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			event, err := m.AddDecisionTaskFailedEvent(scheduledID, startedID, cause, details, identity, reason, binChecksum, baseRunID, newRunID, forkEventVersion, resetRequestID)
			test.assertions(t, event, err, observedLogs, m)
		})
	}
}

func TestAddDecisionTaskCompletedEvent(t *testing.T) {
	var scheduledID int64 = 1
	var startedID int64 = 1
	request := &types.RespondDecisionTaskCompletedRequest{}
	maxResetPoints := 10

	tests := []struct {
		name         string
		assertions   func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl)
		expectations func(m *mutableStateDecisionTaskManagerImpl)
		newMsb       func(t *testing.T) *mutableStateBuilder
	}{
		{
			name: "success",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: scheduledID,
						DecisionStartedID:  startedID,
						DecisionAttempt:    1,
					},
					timeSource:       clock.NewMockedTimeSource(),
					domainEntry:      constants.TestLocalDomainEntry,
					metricsClient:    metrics.NewNoopMetricsClient(),
					config:           config.NewForTest(),
					workflowRequests: make(map[persistence.WorkflowRequest]struct{}),
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.NoError(t, err)
				require.NotNil(t, event)
				assert.Equal(t, scheduledID, event.DecisionTaskCompletedEventAttributes.ScheduledEventID)
				assert.Equal(t, startedID, event.DecisionTaskCompletedEventAttributes.StartedEventID)

			},
		},
		{
			name: "internal server error",
			newMsb: func(t *testing.T) *mutableStateBuilder {
				return &mutableStateBuilder{
					executionInfo: &persistence.WorkflowExecutionInfo{
						DecisionScheduleID: 0, // must be != scheduledID
					},
				}
			},
			assertions: func(t *testing.T, event *types.HistoryEvent, err error, observedLogs *observer.ObservedLogs, m *mutableStateDecisionTaskManagerImpl) {
				require.Error(t, err)
				assert.Equal(t, m.msb.createInternalServerError(tag.WorkflowActionDecisionTaskCompleted), err)
				assert.Nil(t, event)
				assert.Equal(t, 1, observedLogs.FilterMessage(mutableStateInvalidHistoryActionMsg).Len())
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := &mutableStateDecisionTaskManagerImpl{msb: test.newMsb(t)}
			m.msb.hBuilder = NewHistoryBuilder(m.msb)
			m.msb.shard = shard.NewMockContext(gomock.NewController(t))
			core, _ := observer.New(zap.WarnLevel)
			core, observedLogs := observer.New(zap.DebugLevel)
			m.msb.logger = loggerimpl.NewLogger(zap.New(core))
			if test.expectations != nil {
				test.expectations(m)
			}
			event, err := m.AddDecisionTaskCompletedEvent(scheduledID, startedID, request, maxResetPoints)
			test.assertions(t, event, err, observedLogs, m)
		})
	}
}
