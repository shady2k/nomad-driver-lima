// Package scriptassets embeds first-party finite secret-operation contracts.
package scriptassets

import _ "embed"

//go:embed secret_profiles.py
var SecretProfiles string

//go:embed secret_operation_guest.py
var SecretOperation string

//go:embed secret_profile_delivery.py
var SecretDelivery string
