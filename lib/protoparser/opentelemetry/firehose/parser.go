package firehose

import (
	"encoding/json"
	"fmt"
)

// ProcessRequestBody converts Cloudwatch Stream protobuf metrics HTTP request body delivered via Firehose into OpenTelemetry protobuf message.
//
// See https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/CloudWatch-Metric-Streams.html
//
// It joins decoded "data" fields from "record" list:
//
//	{
//	  "requestId": "<uuid-string>",
//	  "timestamp": <int64-value>,
//	  "records": [
//	    {
//	      "data": "<base64-encoded-payload>"
//	    }
//	  ]
//	}
func ProcessRequestBody(b []byte) ([]byte, error) {
	var req struct {
		Records []struct {
			Data []byte
		}
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return nil, fmt.Errorf("cannot unmarshal Firehose JSON in request body: %s", err)
	}

	var dst []byte
	for _, r := range req.Records {
		dst = append(dst, r.Data...)
	}

	return dst, nil
}
