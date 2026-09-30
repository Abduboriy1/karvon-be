package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/bory/karvon-be/internal/registrar/cloudflare"
)

// recordComment marks the records this module wrote, for whoever reads the zone.
const recordComment = "Google Workspace (Karvon)"

// dnsConflict is a record already in the zone that says something else about mail.
// It is never overwritten: a person decides.
type dnsConflict struct{ message string }

func (e *dnsConflict) Error() string { return e.message }

// publishRecords makes the zone carry what Workspace mail needs: Google's MX, an SPF
// record that includes Google, a DMARC policy, and the verification token when there
// is one. Records already right are left alone. Records that explicitly say the
// domain has no mail (a null MX, "v=spf1 -all") are replaced; any other mail record
// is a conflict.
func publishRecords(ctx context.Context, dns *cloudflare.Client, domain string, verificationToken *string) error {
	zone, err := dns.FindZone(ctx, domain)
	if err != nil {
		return err
	}

	// MX
	mx, err := dns.ListDNSRecords(ctx, zone.ID, cloudflare.RecordMX, domain)
	if err != nil {
		return err
	}
	var googleMX bool
	var nullMX *cloudflare.DNSRecord
	var foreign []string
	for i, rec := range mx {
		host := strings.ToLower(strings.TrimSuffix(rec.Content, "."))
		switch {
		case isGoogleMX(host):
			googleMX = true
		case host == "" && len(mx) == 1:
			nullMX = &mx[i]
		default:
			foreign = append(foreign, host)
		}
	}
	if len(foreign) > 0 {
		return &dnsConflict{fmt.Sprintf("%s already has MX records for another mail service (%s); remove them in Cloudflare and retry",
			domain, strings.Join(foreign, ", "))}
	}
	switch {
	case googleMX:
	case nullMX != nil:
		if _, err := dns.UpdateDNSRecordContent(ctx, zone.ID, nullMX.ID, MXHost); err != nil {
			return err
		}
	default:
		priority := MXPriority
		if err := create(ctx, dns, zone.ID, cloudflare.DNSRecord{
			Type: cloudflare.RecordMX, Name: domain, Content: MXHost, Priority: &priority,
		}); err != nil {
			return err
		}
	}

	// SPF and the verification token, both TXT at the apex.
	txt, err := dns.ListDNSRecords(ctx, zone.ID, cloudflare.RecordTXT, domain)
	if err != nil {
		return err
	}
	var spf *cloudflare.DNSRecord
	hasToken := false
	for i, rec := range txt {
		value := cloudflare.TXTValue(rec.Content)
		if strings.HasPrefix(strings.ToLower(value), "v=spf1") {
			if spf != nil {
				return &dnsConflict{fmt.Sprintf("%s has more than one SPF record; keep one in Cloudflare and retry", domain)}
			}
			spf = &txt[i]
		}
		if verificationToken != nil && value == *verificationToken {
			hasToken = true
		}
	}
	switch {
	case spf == nil:
		if err := create(ctx, dns, zone.ID, cloudflare.DNSRecord{Type: cloudflare.RecordTXT, Name: domain, Content: SPFRecord}); err != nil {
			return err
		}
	case strings.Contains(cloudflare.TXTValue(spf.Content), SPFInclude):
	case strings.Join(strings.Fields(cloudflare.TXTValue(spf.Content)), " ") == "v=spf1 -all":
		if _, err := dns.UpdateDNSRecordContent(ctx, zone.ID, spf.ID, SPFRecord); err != nil {
			return err
		}
	default:
		return &dnsConflict{fmt.Sprintf("%s already has an SPF record without Google (%q); add %s to it in Cloudflare and retry",
			domain, cloudflare.TXTValue(spf.Content), SPFInclude)}
	}
	if verificationToken != nil && !hasToken {
		if err := create(ctx, dns, zone.ID, cloudflare.DNSRecord{
			Type: cloudflare.RecordTXT, Name: domain, Content: *verificationToken,
		}); err != nil {
			return err
		}
	}

	// DMARC. Any existing policy is kept: it was chosen on purpose.
	dmarcName := "_dmarc." + domain
	dmarc, err := dns.ListDNSRecords(ctx, zone.ID, cloudflare.RecordTXT, dmarcName)
	if err != nil {
		return err
	}
	for _, rec := range dmarc {
		if strings.HasPrefix(strings.ToUpper(cloudflare.TXTValue(rec.Content)), "V=DMARC1") {
			return nil
		}
	}
	return create(ctx, dns, zone.ID, cloudflare.DNSRecord{Type: cloudflare.RecordTXT, Name: dmarcName, Content: DMARCRecord})
}

// upsertDKIM publishes the DKIM key at selector._domainkey, replacing an older key.
func upsertDKIM(ctx context.Context, dns *cloudflare.Client, domain, selector, value string) error {
	zone, err := dns.FindZone(ctx, domain)
	if err != nil {
		return err
	}
	name := selector + "._domainkey." + domain
	existing, err := dns.ListDNSRecords(ctx, zone.ID, cloudflare.RecordTXT, name)
	if err != nil {
		return err
	}
	for _, rec := range existing {
		current := cloudflare.TXTValue(rec.Content)
		if current == value {
			return nil
		}
		if strings.HasPrefix(current, "v=DKIM1") {
			_, err := dns.UpdateDNSRecordContent(ctx, zone.ID, rec.ID, value)
			return err
		}
	}
	return create(ctx, dns, zone.ID, cloudflare.DNSRecord{Type: cloudflare.RecordTXT, Name: name, Content: value})
}

// create adds a record; one that is already there counts as added.
func create(ctx context.Context, dns *cloudflare.Client, zoneID string, rec cloudflare.DNSRecord) error {
	rec.Comment = recordComment
	if _, err := dns.CreateDNSRecord(ctx, zoneID, rec); err != nil && !cloudflare.IsIdenticalRecord(err) {
		return err
	}
	return nil
}

// isGoogleMX accepts smtp.google.com and the older aspmx.l.google.com set.
func isGoogleMX(host string) bool {
	return host == MXHost || strings.HasSuffix(host, ".l.google.com") || strings.HasSuffix(host, ".googlemail.com")
}
