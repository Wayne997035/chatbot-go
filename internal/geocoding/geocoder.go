package geocoding

import "context"

// Geocoder 地理編碼介面.
type Geocoder interface {
	Geocode(ctx context.Context, query string) ([]GeocodeResult, error)
}
