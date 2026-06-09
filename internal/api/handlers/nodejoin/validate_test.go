// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"errors"
	"testing"
)

func TestValidateAdvertisedEndpointURL(t *testing.T) {
	base := func(endpoint string) joinRequest {
		return joinRequest{
			Token: "otx_join_x", CSRPEM: "pem", NodeName: "node-1", Architecture: "amd64",
			AdvertisedEndpoint: endpoint, MigrationHost: "10.77.0.1",
			MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
		}
	}
	valid := []string{"https://10.77.0.1:9443", "https://node1.example.com:9443", "https://127.0.0.1:9444"}
	for _, e := range valid {
		if err := base(e).validate(); err != nil {
			t.Errorf("validate() advertised_endpoint=%q = %v, want nil", e, err)
		}
	}
	// "" is rejected by the existing errMissingAdvertisedEndpoint (presence), not the URL check.
	badURL := []string{"not-a-url", "http://10.77.0.1:9443", "https://", "://nohost"}
	for _, e := range badURL {
		err := base(e).validate()
		if !errors.Is(err, errAdvertisedEndpointInvalidURL) {
			t.Errorf("validate() advertised_endpoint=%q = %v, want errAdvertisedEndpointInvalidURL", e, err)
		}
	}
}
