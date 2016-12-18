package digitalocean

import (
	"fmt"
	"os"
	"strings"

	"github.com/Sirupsen/logrus"
	api "github.com/digitalocean/godo"
	"golang.org/x/oauth2"

	"github.com/juju/ratelimit"
	"github.com/rancher/external-dns/providers"
	"github.com/rancher/external-dns/utils"
)

type DigitalOceanProvider struct {
	client         *api.Client
	rootDomainName string
	TTL            int
	limiter        *ratelimit.Bucket
}

func init() {
	providers.RegisterProvider("digitalocean", &DigitalOceanProvider{})
}

type TokenSource struct {
	AccessToken string
}

func (t *TokenSource) Token() (*oauth2.Token, error) {
	token := &oauth2.Token{
		AccessToken: t.AccessToken,
	}
	return token, nil
}

func (p *DigitalOceanProvider) Init(rootDomainName string) error {
	var pat string
	if pat = os.Getenv("DO_PAT"); len(pat) == 0 {
		return fmt.Errorf("DO_PAT is not set")
	}

	tokenSource := &TokenSource{
		AccessToken: pat,
	}

	oauthClient := oauth2.NewClient(oauth2.NoContext, tokenSource)
	p.client = api.NewClient(oauthClient)

	// DO's API is rate limited at 5000/hour.
	doqps := (float64)(5000.0 / 3600.0)
	p.limiter = ratelimit.NewBucketWithRate(doqps, 1)

	p.rootDomainName = utils.UnFqdn(rootDomainName)

	// Retrieve email address associated with this PAT.
	p.limiter.Wait(1)
	acct, _, err := p.client.Account.Get()
	if err != nil {
		return err
	}

	// Now confirm that domain is accessible under this PAT.
	p.limiter.Wait(1)
	domains, _, err := p.client.Domains.Get(p.rootDomainName)
	if err != nil {
		return err
	}
	// DO's TTLs are domain-wide.
	p.TTL = domains.TTL

	logrus.Infof("Configured %s for email %s and domain %s", p.GetName(), acct.Email, domains.Name)

	return nil
}

func (p *DigitalOceanProvider) GetName() string {
	return "DigitalOcean"
}

func (p *DigitalOceanProvider) HealthCheck() error {
	p.limiter.Wait(1)
	_, _, err := p.client.Domains.Get(p.rootDomainName)
	return err
}

func (p *DigitalOceanProvider) AddRecord(record utils.DnsRecord) error {
	logrus.Debugf("AddRecord")
	for _, r := range record.Records {
		createRequest := &api.DomainRecordEditRequest{
			Type: record.Type,
			Name: record.Fqdn,
			Data: r,
		}
		logrus.Debugf(" request: %v", createRequest)
		p.limiter.Wait(1)
		rec, _, err := p.client.Domains.CreateRecord(p.rootDomainName, createRequest)
		if err != nil {
			return fmt.Errorf("%s API call has failed: %v", p.GetName(), err)
		}
		logrus.Debugf(" rec: %v", rec)
	}
	return nil
}

func (p *DigitalOceanProvider) UpdateRecord(record utils.DnsRecord) error {
	logrus.Debugf("UpdateRecord")
	if err := p.RemoveRecord(record); err != nil {
		return err
	}
	return p.AddRecord(record)
}

func (p *DigitalOceanProvider) RemoveRecord(record utils.DnsRecord) error {
	logrus.Debugf("RemoveRecord")
	p.limiter.Wait(1)
	records, _, err := p.client.Domains.Records(p.rootDomainName, nil)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.Name == record.Fqdn && rec.Type == record.Type {
			p.limiter.Wait(1)
			_, err := p.client.Domains.DeleteRecord(p.rootDomainName, rec.ID)
			if err != nil {
				return fmt.Errorf("%s API call has failed: %v", p.GetName(), err)
			}
		}
	}
	return err
}

func (p *DigitalOceanProvider) GetRecords() ([]utils.DnsRecord, error) {
	dnsRecords := []utils.DnsRecord{}
	recordMap := map[string]map[string][]string{}
	opt := &api.ListOptions{}
	for {
		p.limiter.Wait(1)
		drecords, resp, err := p.client.Domains.Records(p.rootDomainName, opt)
		if err != nil {
			return nil, fmt.Errorf("%s API call has failed: %v", p.GetName(), err)
		}
		for _, r := range drecords {
			if r.Name == "@" {
				logrus.Debugf("caught @")
				r.Name = p.rootDomainName
			} else {
				names := []string{r.Name, p.rootDomainName}
				r.Name = strings.Join(names, ".")
			}
			fqdn := utils.Fqdn(r.Name)
			recordSet, exists := recordMap[fqdn]
			if exists {
				recordSlice, sliceExists := recordSet[r.Type]
				if sliceExists {
					recordSlice = append(recordSlice, r.Data)
					recordSet[r.Type] = recordSlice
				} else {
					recordSet[r.Type] = []string{r.Data}
				}
			} else {
				recordMap[fqdn] = map[string][]string{}
				recordMap[fqdn][r.Type] = []string{r.Data}
			}
		}
		if resp.Links == nil || resp.Links.IsLastPage() {
			break
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			return nil, fmt.Errorf("%s API call has failed: %v", p.GetName(), err)
		}
		opt.Page = page + 1
	}

	logrus.Debugf("recordSet")
	for fqdn, recordSet := range recordMap {
		for recordType, recordSlice := range recordSet {
			// Digital Ocean does not have per-record TTLs.
			dnsRecord := utils.DnsRecord{Fqdn: fqdn, Records: recordSlice, Type: recordType, TTL: p.TTL}
			logrus.Debugf(" %v", dnsRecord)
			dnsRecords = append(dnsRecords, dnsRecord)
		}
	}

	return dnsRecords, nil
}
