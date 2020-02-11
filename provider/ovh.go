package provider

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ovh/go-ovh/ovh"
	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/plan"
)

// OVHConfig holds connection parameter to connect to the OVH Cloud
type OVHConfig struct {
	Endpoint          string
	ApplicationKey    string
	ApplicationSecret string
	ConsumerKey       string
}

// OvhDomainZoneRecord is a OVH domain zone record object
type OvhDomainZoneRecord struct {
	ID        int64  `json:"id,omitempty"`
	Zone      string `json:"zone,omitempty"`
	Target    string `json:"target"`
	TTL       int    `json:"ttl,omitempty"`
	FieldType string `json:"fieldType"`
	SubDomain string `json:"subDomain,omitempty"`
}

// OVHProvider is an implementation of the Provider interface for OVH
type OVHProvider struct {
	domainFilter DomainFilter
	dryRun       bool
	client       *ovh.Client
}

// NewOVHProvider initialises a new OVH Cloud Provider
func NewOVHProvider(config *OVHConfig, domainFilter DomainFilter, dryRun bool) (*OVHProvider, error) {
	client, err := ovh.NewClient(
		config.Endpoint,
		config.ApplicationKey,
		config.ApplicationSecret,
		config.ConsumerKey,
	)
	if err != nil {
		return nil, err
	}

	provider := OVHProvider{
		domainFilter: domainFilter,
		dryRun:       dryRun,
		client:       client,
	}

	log.Printf("domain filter is: %v", domainFilter)
	log.Printf("dryRun is: %v", dryRun)

	return &provider, nil
}

func (p *OVHProvider) newOvhDomainZoneRecord(zone string, subDomain string, fieldType string, target string, ttl int) *OvhDomainZoneRecord {
	return &OvhDomainZoneRecord{
		Zone:      zone,
		Target:    target,
		TTL:       ttl,
		FieldType: fieldType,
		SubDomain: subDomain,
	}
}

// Records returns the list of records in a given hosted zone.
func (p *OVHProvider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {

	endpoints := []*endpoint.Endpoint{}
	zones, err := p.getZones()
	if err != nil {
		return nil, err
	}

	for _, zone := range zones {
		if p.domainFilter.Match(zone) {
			recordIDs := []int{}
			err := p.client.Get(
				fmt.Sprintf("/domain/zone/%s/record", zone),
				&recordIDs,
			)
			if err != nil {
				return nil, err
			}

			for _, recordID := range recordIDs {
				record := OvhDomainZoneRecord{}
				err := p.client.Get(
					fmt.Sprintf("/domain/zone/%s/record/%d", zone, recordID),
					&record,
				)
				if err != nil {
					return nil, err
				}
				if supportedRecordType(record.FieldType) {
					endpoint := endpoint.NewEndpointWithTTL(
						formatOVHDNSName(record.SubDomain, zone),
						record.FieldType,
						endpoint.TTL(record.TTL),
						record.Target,
					)
					endpoints = append(endpoints, endpoint)
					// log.Printf("Endpoint: %+v", *endpoint)
				}
			}
		}
	}
	return endpoints, nil
}

// ApplyChanges applies a given set of changes to a given zone.
func (p *OVHProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) error {
	zoneNameIDMapper := zoneIDName{}
	zones, err := p.getZones()
	if err != nil {
		return err
	}

	for _, z := range zones {
		zoneNameIDMapper[z] = z
	}

	p.createRecords(zoneNameIDMapper, changes.Create)
	p.deleteRecords(zoneNameIDMapper, changes.Delete)
	p.updateRecords(zoneNameIDMapper, changes.UpdateNew)

	return nil
}

func (p *OVHProvider) createRecords(zoneNameIDMapper zoneIDName, endpoints []*endpoint.Endpoint) {
	for _, endpoint := range endpoints {

		if !p.domainFilter.Match(endpoint.DNSName) {
			log.Printf("Skipping creation at OVH of endpoint DNSName: '%s' RecordType: '%s', it does not match against Domain filters", endpoint.DNSName, endpoint.RecordType)
			continue
		}
		if zoneName, _ := zoneNameIDMapper.FindZone(endpoint.DNSName); zoneName != "" {
			if len(endpoint.Targets) != 1 {
				log.Printf("Cannot create OVH of endpoint DNSName: '%s' RecordType: '%s', cannot have multiple Targets", endpoint.DNSName, endpoint.RecordType)
				continue
			}

			record := OvhDomainZoneRecord{
				Target:    endpoint.Targets[0],
				TTL:       int(endpoint.RecordTTL),
				FieldType: endpoint.RecordType,
				SubDomain: getOVHSubDomain(endpoint.DNSName, zoneName),
			}

			log.Printf("Create new Endpoint at OVH - Zone: '%s', DNSName: '%s', RecordType: '%s', Targets: '%+v'", zoneName, endpoint.DNSName, endpoint.RecordType, endpoint.Targets)

			if p.dryRun {
				continue
			}

			newRecord := OvhDomainZoneRecord{}
			err := p.client.Post(
				fmt.Sprintf("/domain/zone/%s/record", zoneName),
				&record,
				&newRecord,
			)
			if err != nil {
				log.Printf("Failed to create OVH endpoint DNSName: '%s' RecordType: '%s' for zone: '%s'", endpoint.DNSName, endpoint.RecordType, zoneName)
				log.Printf("Error was: %s", err)
				continue
			}
		} else {
			log.Printf("No matching zone for endpoint addition DNSName: '%s' RecordType: '%s'", endpoint.DNSName, endpoint.RecordType)
		}
	}
}

func (p *OVHProvider) deleteRecords(zoneNameIDMapper zoneIDName, endpoints []*endpoint.Endpoint) {
	for _, endpoint := range endpoints {

		if !p.domainFilter.Match(endpoint.DNSName) {
			log.Printf("Skipping delete at OVH of endpoint DNSName: '%s' RecordType: '%s', it does not match against Domain filters", endpoint.DNSName, endpoint.RecordType)
			continue
		}
		if zoneName, _ := zoneNameIDMapper.FindZone(endpoint.DNSName); zoneName != "" {
			ids := []int{}
			err := p.client.Get(
				fmt.Sprintf("/domain/zone/%s/record?fieldType=%s&subDomain=%s", zoneName, endpoint.RecordType, getOVHSubDomain(endpoint.DNSName, zoneName)),
				&ids,
			)
			if err != nil || len(ids) != 1 {
				log.Printf("Cannot to delete OVH endpoint DNSName: '%s' RecordType: '%s', Endpoint does not exist or id is ambiguous", endpoint.DNSName, endpoint.RecordType)
				log.Printf("Error was: %s", err)
				continue
			}

			log.Printf("Delete Endpoint at OVH - Zone: '%s', DNSName: '%s', RecordType: '%s', Targets: '%+v'", zoneName, endpoint.DNSName, endpoint.RecordType, endpoint.Targets)

			if p.dryRun {
				continue
			}

			err = p.client.Delete(
				fmt.Sprintf("/domain/zone/%s/record/%d", zoneName, ids[0]),
				nil,
			)
			if err != nil {
				log.Printf("Failed to delete OVH endpoint DNSName: '%s' RecordType: '%s' for zone: '%s'", endpoint.DNSName, endpoint.RecordType, zoneName)
				log.Printf("Error was: %s", err)
				continue
			}
		} else {
			log.Printf("No matching zone for endpoint addition DNSName: '%s' RecordType: '%s'", endpoint.DNSName, endpoint.RecordType)
		}
	}
}

func (p *OVHProvider) updateRecords(zoneNameIDMapper zoneIDName, endpoints []*endpoint.Endpoint) {
	for _, endpoint := range endpoints {

		if !p.domainFilter.Match(endpoint.DNSName) {
			log.Printf("Skipping update at OVH of endpoint DNSName: '%s' RecordType: '%s', it does not match against Domain filters", endpoint.DNSName, endpoint.RecordType)
			continue
		}
		if zoneName, _ := zoneNameIDMapper.FindZone(endpoint.DNSName); zoneName != "" {
			if len(endpoint.Targets) != 1 {
				log.Printf("Cannot update OVH of endpoint DNSName: '%s' RecordType: '%s', cannot have multiple Targets", endpoint.DNSName, endpoint.RecordType)
				continue
			}

			ids := []int{}
			err := p.client.Get(
				fmt.Sprintf("/domain/zone/%s/record?fieldType=%s&subDomain=%s", zoneName, endpoint.RecordType, getOVHSubDomain(endpoint.DNSName, zoneName)),
				&ids,
			)
			if err != nil || len(ids) != 1 {
				log.Printf("Cannot to delete OVH endpoint DNSName: '%s' RecordType: '%s', Endpoint does not exist or id is ambiguous", endpoint.DNSName, endpoint.RecordType)
				log.Printf("Error was: %s", err)
				continue
			}

			record := OvhDomainZoneRecord{
				Target:    endpoint.Targets[0],
				TTL:       int(endpoint.RecordTTL),
				FieldType: endpoint.RecordType,
				SubDomain: getOVHSubDomain(endpoint.DNSName, zoneName),
			}

			log.Printf("Update Endpoint at OVH - Zone: '%s', DNSName: '%s', RecordType: '%s', Targets: '%+v'", zoneName, endpoint.DNSName, endpoint.RecordType, endpoint.Targets)

			if p.dryRun {
				continue
			}

			err = p.client.Put(
				fmt.Sprintf("/domain/zone/%s/record/%d", zoneName, ids[0]),
				record,
				nil,
			)
			if err != nil {
				log.Printf("Failed to update OVH endpoint DNSName: '%s' RecordType: '%s' for zone: '%s'", endpoint.DNSName, endpoint.RecordType, zoneName)
				log.Printf("Error was: %s", err)
				continue
			}
		} else {
			log.Printf("No matching zone for endpoint addition DNSName: '%s' RecordType: '%s'", endpoint.DNSName, endpoint.RecordType)
		}
	}
}

func formatOVHDNSName(recordName, zoneName string) string {
	if recordName == "" {
		return zoneName
	}
	return fmt.Sprintf("%s.%s", recordName, zoneName)
}

func getOVHSubDomain(DNSName, zoneName string) string {
	subDomain := ""
	if DNSName != zoneName {
		subDomain = strings.TrimSuffix(DNSName, "."+zoneName)
	}
	return subDomain
}

func (p *OVHProvider) getZones() ([]string, error) {
	zones := []string{}
	err := p.client.Get(
		"/domain/zone",
		&zones,
	)
	if err != nil {
		return nil, err
	}
	return zones, nil
}
