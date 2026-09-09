/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

package bedrockruntime_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"chainguard.dev/driftlessaf/agents/awsauth"
	"chainguard.dev/driftlessaf/agents/internal/bedrockruntime"
)

func ExampleValidRegion() {
	fmt.Println(bedrockruntime.ValidRegion("us-west-2"))
	fmt.Println(bedrockruntime.ValidRegion("us-gov-west-1"))
	// Output:
	// true
	// false
}

func ExampleNew() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client, err := bedrockruntime.New(ctx, awsauth.Config{Region: "us-west-2", Profile: "engineering-sso"})
	if err != nil {
		return
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Endpoint()+"/openai/v1/responses",
		strings.NewReader(`{"model":"us.openai.gpt-5.6-terra","input":"Hello","store":false}`))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return
	}
	defer response.Body.Close()
	// The protocol adapter consumes response.Body and the AWS request ID.
}
