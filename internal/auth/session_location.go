package auth

import (
	"net"
	"strings"

	"github.com/oschwald/geoip2-golang"
)

const (
	unknownSessionLocation = "\u672a\u77e5\u5730\u70b9"
	localSessionLocation   = "\u672c\u673a"
	privateSessionLocation = "\u5185\u7f51"
)

type SessionLocationResolver struct {
	db *geoip2.Reader
}

func NewSessionLocationResolver(databasePath string) (*SessionLocationResolver, error) {
	databasePath = strings.TrimSpace(databasePath)
	if databasePath == "" {
		return &SessionLocationResolver{}, nil
	}

	db, err := geoip2.Open(databasePath)
	if err != nil {
		return nil, err
	}
	return &SessionLocationResolver{db: db}, nil
}

func (r *SessionLocationResolver) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

func (r *SessionLocationResolver) Location(ipValue *string) string {
	ip := parseSessionIP(ipValue)
	if ip == nil {
		return unknownSessionLocation
	}
	if ip.IsLoopback() {
		return localSessionLocation
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return privateSessionLocation
	}
	if r == nil || r.db == nil {
		return unknownSessionLocation
	}

	if location := r.cityLocation(ip); location != "" {
		return location
	}
	if location := r.countryLocation(ip); location != "" {
		return location
	}
	return unknownSessionLocation
}

func (r *SessionLocationResolver) cityLocation(ip net.IP) string {
	record, err := r.db.City(ip)
	if err != nil || record == nil {
		return ""
	}
	country := localizedGeoName(record.Country.Names, record.Country.IsoCode)
	parts := []string{country}
	for _, subdivision := range record.Subdivisions {
		parts = append(
			parts,
			localizedGeoName(subdivision.Names, subdivision.IsoCode),
		)
	}
	parts = append(parts, localizedGeoName(record.City.Names, ""))
	if postalCode := strings.TrimSpace(record.Postal.Code); postalCode != "" {
		parts = append(parts, "邮编 "+postalCode)
	}
	return joinLocationParts(parts...)
}

func (r *SessionLocationResolver) countryLocation(ip net.IP) string {
	record, err := r.db.Country(ip)
	if err != nil || record == nil {
		return ""
	}
	return localizedGeoName(record.Country.Names, record.Country.IsoCode)
}

func parseSessionIP(value *string) net.IP {
	if value == nil {
		return nil
	}
	raw := strings.TrimSpace(*value)
	if raw == "" {
		return nil
	}
	if strings.EqualFold(raw, "localhost") {
		return net.ParseIP("127.0.0.1")
	}
	return net.ParseIP(raw)
}

func localizedGeoName(names map[string]string, fallback string) string {
	for _, language := range []string{"zh-CN", "zh", "en"} {
		if name := strings.TrimSpace(names[language]); name != "" {
			return name
		}
	}
	return strings.TrimSpace(fallback)
}

func joinLocationParts(parts ...string) string {
	unique := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		unique = append(unique, part)
	}
	return strings.Join(unique, ", ")
}
