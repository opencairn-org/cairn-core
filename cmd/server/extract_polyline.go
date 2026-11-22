package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newExtractPolylineCmd() *cobra.Command {
	var activityId string
	var startTime string
	var endTime string
	cmd := &cobra.Command{
		Use:   "extract-polyline",
		Short: "Extract polyline from an activity to create a segment",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runExtractPolyline(cmd.Context(), activityId, startTime, endTime)
		},
	}
	cmd.Flags().StringVarP(&activityId, "activity-id", "a", "", "UUID of the activity")
	cmd.Flags().StringVarP(&startTime, "start", "s", "", "start time for the segment, ex: 2026-01-02T15:04:00-03:00 or 2026-11-22T18:00:00Z for UTC (which is what we have in the GPX files)")
	cmd.Flags().StringVarP(&endTime, "end", "e", "", "end time for the segment")
	cmd.MarkFlagRequired("activity-id")
	cmd.MarkFlagRequired("start")
	cmd.MarkFlagRequired("end")
	return cmd
}

func runExtractPolyline(context context.Context, activityId, startTime, endTime string) error {
	start, err := time.Parse(time.RFC3339, startTime)
	if err != nil {
		return fmt.Errorf("invalid start time (want RFC3339): %w", err)
	}
	end, err := time.Parse(time.RFC3339, endTime)
	if err != nil {
		return fmt.Errorf("invalid end time (want RFC3339): %w", err)
	}
	fmt.Printf("Will extract segment from activity %s, from %s to %s", activityId, start, end)
	return nil
}
