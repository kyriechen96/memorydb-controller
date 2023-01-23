// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cluster

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	ackrequeue "github.com/aws-controllers-k8s/runtime/pkg/requeue"
	ackrtlog "github.com/aws-controllers-k8s/runtime/pkg/runtime/log"
	ackutil "github.com/aws-controllers-k8s/runtime/pkg/util"
	svcsdk "github.com/aws/aws-sdk-go/service/memorydb"

	svcapitypes "github.com/aws-controllers-k8s/memorydb-controller/apis/v1alpha1"
)

var (
	condMsgCurrentlyDeleting            = "cluster currently being deleted"
	condMsgNoDeleteWhileUpdating        = "cluster is being updated. cannot delete"
	resourceStatusActive         string = "active"
)

var (
	requeueWaitWhileDeleting = ackrequeue.NeededAfter(
		errors.New("delete is in progress"),
		ackrequeue.DefaultRequeueAfterDuration,
	)
	requeueWaitWhileUpdating = ackrequeue.NeededAfter(
		errors.New("update is in progress"),
		ackrequeue.DefaultRequeueAfterDuration,
	)
)

const (
	StatusAvailable    = "available"
	StatusDeleting     = "deleting"
	StatusUpdating     = "updating"
	StatusCreateFailed = "create-failed"
)

// isDeleting returns true if supplied cluster resource state is 'deleting'
func isDeleting(r *resource) bool {
	if r == nil || r.ko.Status.Status == nil {
		return false
	}
	status := *r.ko.Status.Status
	return status == StatusDeleting
}

// isUpdating returns true if supplied cluster resource state is 'modifying'
func isUpdating(r *resource) bool {
	if r == nil || r.ko.Status.Status == nil {
		return false
	}
	status := *r.ko.Status.Status
	return status == StatusUpdating
}

func (rm *resourceManager) setShardDetails(
	ctx context.Context,
	r *resource,
	ko *svcapitypes.Cluster,
) (*svcapitypes.Cluster, error) {

	resp, err := rm.sdkFind(ctx, r)

	if err != nil {
		return nil, err
	}

	ko.Status = resp.ko.Status
	ko.Spec.NumReplicasPerShard = resp.ko.Spec.NumReplicasPerShard
	ko.Spec.NumShards = resp.ko.Spec.NumShards

	return ko, nil
}

func (rm *resourceManager) setAllowedNodeTypeUpdates(
	ctx context.Context,
	ko *svcapitypes.Cluster,
) error {
	if *ko.Status.Status != StatusAvailable {
		return nil
	}

	response, respErr := rm.sdkapi.ListAllowedNodeTypeUpdatesWithContext(ctx, &svcsdk.ListAllowedNodeTypeUpdatesInput{
		ClusterName: ko.Spec.Name,
	})
	rm.metrics.RecordAPICall("GET", "ListAllowedNodeTypeUpdates", respErr)
	if respErr == nil {
		ko.Status.AllowedScaleDownNodeTypes = response.ScaleDownNodeTypes
		ko.Status.AllowedScaleUpNodeTypes = response.ScaleUpNodeTypes
	}

	return respErr
}

// validateClusterNeedsUpdate this function's purpose is to requeue if the resource is currently unavailable and
// to validate if resource update is required.
func (rm *resourceManager) validateClusterNeedsUpdate(
	desired *resource,
	latest *resource,
	delta *ackcompare.Delta,
) (*resource, error) {
	// requeue if necessary
	latestStatus := latest.ko.Status.Status
	if latestStatus == nil || *latestStatus != StatusAvailable {
		return nil, ackrequeue.NeededAfter(
			fmt.Errorf("cluster cannot be updated as its status is not '%s'", StatusAvailable),
			ackrequeue.DefaultRequeueAfterDuration)
	}

	// Set terminal condition when cluster is in create-failed state
	if *latestStatus == StatusCreateFailed {
		return nil, ackerr.NewTerminalError(fmt.Errorf("cluster is in '%s' state, cannot be updated", StatusCreateFailed))
	}

	annotations := desired.ko.ObjectMeta.GetAnnotations()

	// Handle asynchronous rollback of NodeType. This can happen due to ICE, InsufficientMemoryException or other
	// errors TODO update the error message once we add describe events support
	if val, ok := annotations[AnnotationLastRequestedNodeType]; ok && desired.ko.Spec.NodeType != nil {
		if val == *desired.ko.Spec.NodeType && delta.DifferentAt("Spec.NodeType") {
			return nil, ackerr.NewTerminalError(errors.New("cannot update NodeType"))
		}
	}

	// Handle asynchronous rollback of NumShards. This can happen due to ICE, InsufficientMemoryException or other
	// errors TODO update the error message once we add describe events support
	if val, ok := annotations[AnnotationLastRequestedNumShards]; ok && desired.ko.Spec.NumShards != nil {
		numShards, err := strconv.ParseInt(val, 10, 64)
		if err == nil && numShards == *desired.ko.Spec.NumShards && delta.DifferentAt("Spec.NumShards") {
			return nil, ackerr.NewTerminalError(errors.New("cannot update NumShards"))
		}
	}

	// Handle asynchronous rollback of NumShards. This can happen due to ICE, InsufficientMemoryException or other
	// errors TODO update the error message once we add describe events support
	if val, ok := annotations[AnnotationLastRequestedNumReplicasPerShard]; ok && desired.ko.Spec.NumReplicasPerShard != nil {
		numReplicasPerShard, err := strconv.ParseInt(val, 10, 64)
		if err == nil && numReplicasPerShard == *desired.ko.Spec.NumReplicasPerShard && delta.DifferentAt("Spec.NumReplicasPerShard") {
			return nil, ackerr.NewTerminalError(errors.New("cannot update NumReplicasPerShard"))
		}
	}

	return nil, nil
}

func (rm *resourceManager) newMemoryDBClusterUploadPayload(
	desired *resource,
	latest *resource,
	delta *ackcompare.Delta,
) *svcsdk.UpdateClusterInput {
	res := &svcsdk.UpdateClusterInput{}

	if delta.DifferentAt("Spec.ACLName") && desired.ko.Spec.ACLName != nil {
		res.SetACLName(*desired.ko.Spec.ACLName)
	}
	if desired.ko.Spec.Name != nil {
		res.SetClusterName(*desired.ko.Spec.Name)
	}
	if delta.DifferentAt("Spec.Description") && desired.ko.Spec.Description != nil {
		res.SetDescription(*desired.ko.Spec.Description)
	}
	if delta.DifferentAt("Spec.MaintenanceWindow") && desired.ko.Spec.MaintenanceWindow != nil {
		res.SetMaintenanceWindow(*desired.ko.Spec.MaintenanceWindow)
	}
	if delta.DifferentAt("Spec.ParameterGroupName") && desired.ko.Spec.ParameterGroupName != nil {
		res.SetParameterGroupName(*desired.ko.Spec.ParameterGroupName)
	}
	if delta.DifferentAt("Spec.SecurityGroupIDs") && desired.ko.Spec.SecurityGroupIDs != nil {
		f8 := []*string{}
		for _, f8iter := range desired.ko.Spec.SecurityGroupIDs {
			var f8elem string
			f8elem = *f8iter
			f8 = append(f8, &f8elem)
		}
		res.SetSecurityGroupIds(f8)
	}
	if delta.DifferentAt("Spec.SnapshotRetentionLimit") && desired.ko.Spec.SnapshotRetentionLimit != nil {
		res.SetSnapshotRetentionLimit(*desired.ko.Spec.SnapshotRetentionLimit)
	}
	if delta.DifferentAt("Spec.SnapshotWindow") && desired.ko.Spec.SnapshotWindow != nil {
		res.SetSnapshotWindow(*desired.ko.Spec.SnapshotWindow)
	}
	if delta.DifferentAt("Spec.SNSTopicARN") && desired.ko.Spec.SNSTopicARN != nil {
		res.SetSnsTopicArn(*desired.ko.Spec.SNSTopicARN)
	}
	if delta.DifferentAt("Spec.SNSTopicStatus") && desired.ko.Status.SNSTopicStatus != nil {
		res.SetSnsTopicStatus(*desired.ko.Status.SNSTopicStatus)
	}

	if delta.DifferentAt("Spec.EngineVersion") && desired.ko.Spec.EngineVersion != nil {
		res.SetEngineVersion(*desired.ko.Spec.EngineVersion)
	}

	// Determine if we are trying to scale up an instance
	scaleUpUpdate := false
	if delta.DifferentAt("Spec.NodeType") && desired.ko.Spec.NodeType != nil {
		for _, instance := range desired.ko.Status.AllowedScaleUpNodeTypes {
			if *instance == *desired.ko.Spec.NodeType {
				scaleUpUpdate = true
				break
			}
		}
	}

	// Determine if we are doing scaling up/down along with resharding
	if delta.DifferentAt("Spec.NodeType") && desired.ko.Spec.NodeType != nil &&
		delta.DifferentAt("Spec.NumShards") {
		if latest.ko.Spec.NumShards != nil && desired.ko.Spec.NumShards != nil {
			// If we are scaling in, then perform scale up or down it does not matter.
			if *latest.ko.Spec.NumShards > *desired.ko.Spec.NumShards {
				scaleUpUpdate = true
			}
		}
	}

	// This means we are not scaling out, so we can perform scale up/down. Reason we give preference to scale down
	// instead of scale in is we perform scale down and update engine version together.
	if scaleUpUpdate {
		res.SetNodeType(*desired.ko.Spec.NodeType)
	}

	engineUpgradeOrScaling := delta.DifferentAt("Spec.EngineVersion") || scaleUpUpdate

	if !engineUpgradeOrScaling && delta.DifferentAt("Spec.NumShards") && desired.ko.Spec.NumShards != nil {
		shardConfig := &svcsdk.ShardConfigurationRequest{}
		shardConfig.SetShardCount(*desired.ko.Spec.NumShards)
		res.SetShardConfiguration(shardConfig)
	}

	reSharding := delta.DifferentAt("Spec.NumShards")

	// Ensure no resharding would be done in API call
	if !reSharding && delta.DifferentAt("Spec.NodeType") && desired.ko.Spec.NodeType != nil {
		res.SetNodeType(*desired.ko.Spec.NodeType)
	}

	// If no scaling or engine upgrade then perform replica scaling.
	engineUpgradeOrScaling = delta.DifferentAt("Spec.EngineVersion") || delta.DifferentAt("Spec.NodeType")

	if !engineUpgradeOrScaling && !reSharding &&
		delta.DifferentAt("Spec.NumReplicasPerShard") && desired.ko.Spec.NumReplicasPerShard != nil {
		replicaConfig := &svcsdk.ReplicaConfigurationRequest{}
		replicaConfig.SetReplicaCount(*desired.ko.Spec.NumReplicasPerShard)
		res.SetReplicaConfiguration(replicaConfig)
	}

	return res
}

// clusterActive returns true when the status of the given Cluster is set to `active`
func (rm *resourceManager) clusterActive(
	latest *resource,
) bool {
	latestStatus := latest.ko.Status.Status
	return latestStatus != nil && *latestStatus == resourceStatusActive
}

// getTags gets tags from given ParameterGroup.
func (rm *resourceManager) getTags(
	ctx context.Context,
	resourceARN string,
) ([]*svcapitypes.Tag, error) {
	resp, err := rm.sdkapi.ListTagsWithContext(
		ctx,
		&svcsdk.ListTagsInput{
			ResourceArn: &resourceARN,
		},
	)
	rm.metrics.RecordAPICall("GET", "ListTags", err)
	if err != nil {
		return nil, err
	}
	tags := resourceTagsFromSDKTags(resp.TagList)
	return tags, nil
}

// updateTags updates tags of given ParameterGroup to desired tags.
func (rm *resourceManager) updateTags(
	ctx context.Context,
	desired *resource,
	latest *resource,
) (err error) {
	rlog := ackrtlog.FromContext(ctx)
	exit := rlog.Trace("rm.updateTags")
	defer func(err error) { exit(err) }(err)

	arn := (*string)(latest.ko.Status.ACKResourceMetadata.ARN)

	toAdd, toDelete := computeTagsDelta(
		desired.ko.Spec.Tags, latest.ko.Spec.Tags,
	)

	if len(toDelete) > 0 {
		rlog.Debug("removing tags from cluster", "tags", toDelete)
		_, err = rm.sdkapi.UntagResourceWithContext(
			ctx,
			&svcsdk.UntagResourceInput{
				ResourceArn: arn,
				TagKeys:     toDelete,
			},
		)
		rm.metrics.RecordAPICall("UPDATE", "UntagResource", err)
		if err != nil {
			return err
		}
	}

	if len(toAdd) > 0 {
		rlog.Debug("adding tags to cluster", "tags", toAdd)
		_, err = rm.sdkapi.TagResourceWithContext(
			ctx,
			&svcsdk.TagResourceInput{
				ResourceArn: arn,
				Tags:        sdkTagsFromResourceTags(toAdd),
			},
		)
		rm.metrics.RecordAPICall("UPDATE", "TagResource", err)
		if err != nil {
			return err
		}
	}

	return nil
}

func computeTagsDelta(
	desired []*svcapitypes.Tag,
	latest []*svcapitypes.Tag,
) (addedOrUpdated []*svcapitypes.Tag, removed []*string) {
	var visitedIndexes []string
	var hasSameKey bool

	for _, latestElement := range latest {
		hasSameKey = false
		visitedIndexes = append(visitedIndexes, *latestElement.Key)
		for _, desiredElement := range desired {
			if equalStrings(latestElement.Key, desiredElement.Key) {
				hasSameKey = true
				if !equalStrings(latestElement.Value, desiredElement.Value) {
					addedOrUpdated = append(addedOrUpdated, desiredElement)
				}
				break
			}
		}
		if hasSameKey {
			continue
		}
		removed = append(removed, latestElement.Key)
	}
	for _, desiredElement := range desired {
		if !ackutil.InStrings(*desiredElement.Key, visitedIndexes) {
			addedOrUpdated = append(addedOrUpdated, desiredElement)
		}
	}
	return addedOrUpdated, removed
}

func sdkTagsFromResourceTags(
	rTags []*svcapitypes.Tag,
) []*svcsdk.Tag {
	tags := make([]*svcsdk.Tag, len(rTags))
	for i := range rTags {
		tags[i] = &svcsdk.Tag{
			Key:   rTags[i].Key,
			Value: rTags[i].Value,
		}
	}
	return tags
}

func resourceTagsFromSDKTags(
	sdkTags []*svcsdk.Tag,
) []*svcapitypes.Tag {
	tags := make([]*svcapitypes.Tag, len(sdkTags))
	for i := range sdkTags {
		tags[i] = &svcapitypes.Tag{
			Key:   sdkTags[i].Key,
			Value: sdkTags[i].Value,
		}
	}
	return tags
}

func equalStrings(a, b *string) bool {
	if a == nil {
		return b == nil
	} else if b == nil {
		return false
	}
	return *a == *b
}
