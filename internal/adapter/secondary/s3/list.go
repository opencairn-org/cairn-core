package s3

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/opencairn-org/cairn-core/internal/port"
)

var _ port.BlobLister = (*BlobStore)(nil)

// ListBlobKeys walks every object under prefix, page by page.
func (b *BlobStore) ListBlobKeys(ctx context.Context, prefix string, visit func(key string, modified time.Time) error) error {
	pager := awss3.NewListObjectsV2Paginator(b.client, &awss3.ListObjectsV2Input{Bucket: aws.String(b.cfg.Bucket), Prefix: aws.String(prefix)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			if err := visit(*obj.Key, aws.ToTime(obj.LastModified)); err != nil {
				return err
			}
		}
	}
	return nil
}
