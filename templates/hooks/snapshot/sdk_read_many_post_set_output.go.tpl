    // custom set output from response
    ko, err = rm.customDescribeSnapshotSetOutput(resp, ko)
    if err != nil {
    	return nil, err
    }

    resourceARN := (*string)(ko.Status.ACKResourceMetadata.ARN)
    tags, err := rm.getTags(ctx, *resourceARN)
    if err != nil {
        return nil, err
    }
    ko.Spec.Tags = tags