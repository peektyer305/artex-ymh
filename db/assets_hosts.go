package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// =====================================================================
// UpsertRootDomain
// =====================================================================

// UpsertRootDomainReq is the input for UpsertRootDomain.
type UpsertRootDomainReq struct {
	Domain string
	ICP    string
	TaskID int64
}

// UpsertRootDomain idempotently inserts or merges a root domain asset.
func (s *AssetStore) UpsertRootDomain(req UpsertRootDomainReq) (int64, error) {
	domain := DomainKey(req.Domain)
	if domain == "" {
		return 0, fmt.Errorf("domain is required")
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertRootDomain(req)
		})
	}
	companyID, err := s.resolveCompanyWithICP(domain, "", req.ICP)
	if err != nil {
		return 0, err
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	var icpVal any
	if req.ICP != "" {
		icpVal = req.ICP
	}

	var id int64
	err = s.tx.QueryRow(`
INSERT INTO assets(type, domain, root_domain, icp, company_id, company_source, task_ids)
VALUES ('root_domain', $1, $1, $2, $3, 'scope', $4::bigint[])
ON CONFLICT (domain) WHERE type = 'root_domain' DO UPDATE SET
    icp        = COALESCE(EXCLUDED.icp, assets.icp),
    company_id = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    task_ids   = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    extra      = assets.extra || EXCLUDED.extra,
    last_seen  = now()
RETURNING id`, domain, icpVal, companyID, taskIDs).Scan(&id)
	return id, err
}

// ErrAssetIPInvalid marks a non-address value in an asset's ip field. Both
// insert_assets and the asset API report it per item with the item index, so one
// bad entry never costs the rest of the batch.
var ErrAssetIPInvalid = errors.New("invalid asset ip")

// ValidateAssetIP keeps hostnames out of assets.ip. Network attribution casts
// that column to inet (see try_inet in schema.sql), so a hostname stored here is
// silently invisible to every IP/CIDR scope rule — the asset simply never gets
// attributed and nobody can tell why. The message states the fix rather than
// just the fault, so an agent can correct the item on its next turn. An empty
// value is accepted: the ip field is optional for service and endpoint assets.
func ValidateAssetIP(value string) error {
	if value == "" || net.ParseIP(value) != nil {
		return nil
	}
	return fmt.Errorf(
		"%w: ip は IPv4/IPv6 アドレスである必要があります。受信値は %q です。これがホスト名なら type=subdomain に変更して domain フィールドを埋めてください。"+
			"アドレスを登録したい場合は、まず A/AAAA レコードを解決し、解決したアドレスを ip に入力してください",
		ErrAssetIPInvalid, value)
}

// =====================================================================
// UpsertIP
// =====================================================================

// UpsertIPReq is the input for UpsertIP.
type UpsertIPReq struct {
	IP           string
	BoundDomains []string
	OpenPorts    []PortService
	TaskID       int64
}

// UpsertIP idempotently inserts or merges an IP asset.
func (s *AssetStore) UpsertIP(req UpsertIPReq) (int64, error) {
	if req.IP == "" {
		return 0, fmt.Errorf("ip is required")
	}
	if err := ValidateAssetIP(req.IP); err != nil {
		return 0, err
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertIP(req)
		})
	}
	cseg := calcCSegment(req.IP)
	companyID, err := s.resolveCompanyWithICP("", req.IP, "")
	if err != nil {
		return 0, err
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	boundDomains := marshalStringArray(req.BoundDomains)
	openPortsJSON, err := marshalPortServices(req.OpenPorts)
	if err != nil {
		return 0, err
	}

	var csegVal any
	if cseg != "" {
		csegVal = cseg
	}

	var id int64
	err = s.tx.QueryRow(`
INSERT INTO assets(type, ip, c_segment, bound_domains, open_ports, company_id, company_source, task_ids)
VALUES ('ip', $1, $2::cidr, $3::text[], $4::jsonb[], $5, 'scope', $6::bigint[])
ON CONFLICT (ip) WHERE type = 'ip' DO UPDATE SET
    bound_domains = (SELECT ARRAY(SELECT DISTINCT unnest(assets.bound_domains || EXCLUDED.bound_domains))),
    open_ports    = (
        SELECT ARRAY(
            SELECT DISTINCT ON ((elem->>'port')::int) elem
            FROM unnest(assets.open_ports || EXCLUDED.open_ports) AS elem
            ORDER BY (elem->>'port')::int,
                     CASE WHEN elem->>'service' IS NOT NULL AND elem->>'service' <> '' THEN 0 ELSE 1 END
        )
    ),
    c_segment  = COALESCE(assets.c_segment, EXCLUDED.c_segment),
    company_id = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    task_ids   = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    last_seen  = now()
RETURNING id`, req.IP, csegVal, boundDomains, openPortsJSON, companyID, taskIDs).Scan(&id)
	return id, err
}

// AppendIPPort appends a {port, service} entry to an existing IP asset's open_ports.
func (s *AssetStore) AppendIPPort(ipStr string, port int, serviceName string) error {
	if ipStr == "" || port == 0 {
		return nil
	}
	entry, _ := json.Marshal(map[string]any{"port": port, "service": serviceName})
	_, err := s.db.Exec(`
UPDATE assets SET
    open_ports = (
        SELECT ARRAY(
            SELECT DISTINCT ON ((elem->>'port')::int) elem
            FROM unnest(open_ports || ARRAY[$1::jsonb]) AS elem
            ORDER BY (elem->>'port')::int,
                     CASE WHEN elem->>'service' IS NOT NULL AND elem->>'service' <> '' THEN 0 ELSE 1 END
        )
    ),
    last_seen = now()
WHERE type = 'ip' AND ip = $2`, string(entry), ipStr)
	return err
}

// AppendIPBoundDomain appends a domain to an existing IP asset's bound_domains.
func (s *AssetStore) AppendIPBoundDomain(ipStr, domain string) error {
	if ipStr == "" || domain == "" {
		return nil
	}
	_, err := s.db.Exec(`
UPDATE assets SET
    bound_domains = (SELECT ARRAY(SELECT DISTINCT unnest(bound_domains || ARRAY[$1::text]))),
    last_seen = now()
WHERE type = 'ip' AND ip = $2`, domain, ipStr)
	return err
}

// =====================================================================
// UpsertSubdomain
// =====================================================================

// UpsertSubdomainReq is the input for UpsertSubdomain.
type UpsertSubdomainReq struct {
	Domain      string
	RecordType  string
	RecordValue []string
	ICP         string
	TaskID      int64
}

// UpsertSubdomain idempotently inserts or merges a subdomain asset and triggers
// side effects: root_domain upsert + IP bound_domains update.
func (s *AssetStore) UpsertSubdomain(req UpsertSubdomainReq) (id int64, err error) {
	domain := DomainKey(req.Domain)
	if domain == "" {
		return 0, fmt.Errorf("domain is required")
	}
	if s.tx == nil {
		return s.withCompanyScopeMutation(func(scoped *AssetStore) (int64, error) {
			return scoped.UpsertSubdomain(req)
		})
	}

	rootDomain, _ := RootDomain(domain)
	if rootDomain == "" {
		rootDomain = domain
	}

	// Resolve by root domain first, then the asset's exact normalized ICP.
	companyID, err := s.resolveCompanyWithICP(rootDomain, "", req.ICP)
	if err != nil {
		return 0, err
	}

	// side effect 1: ensure root domain exists
	_, _ = s.UpsertRootDomain(UpsertRootDomainReq{
		Domain: rootDomain,
		TaskID: req.TaskID,
	})

	// side effect 2: if A/AAAA record, upsert each IP + bind domain.
	// RecordValue is []string; each element may itself be comma-separated (legacy).
	var ipStr string // first valid IP, used for the subdomain row itself
	if req.RecordType == "A" || req.RecordType == "AAAA" {
		for _, rv := range req.RecordValue {
			for _, part := range strings.Split(rv, ",") {
				candidate := strings.TrimSpace(part)
				if candidate == "" || net.ParseIP(candidate) == nil {
					continue
				}
				if ipStr == "" {
					ipStr = candidate
				}
				_, _ = s.UpsertIP(UpsertIPReq{
					IP:           candidate,
					BoundDomains: []string{domain},
					TaskID:       req.TaskID,
				})
			}
		}
	}

	cseg := calcCSegment(ipStr)
	if companyID == nil && ipStr != "" {
		companyID, _ = s.resolveCompanyWithICP("", ipStr, "")
	}

	var taskIDs string
	if req.TaskID > 0 {
		taskIDs = fmt.Sprintf("{%d}", req.TaskID)
	} else {
		taskIDs = "{}"
	}

	var icpVal, csegVal, ipVal any
	if req.ICP != "" {
		icpVal = req.ICP
	}
	if cseg != "" {
		csegVal = cseg
	}
	if ipStr != "" {
		ipVal = ipStr
	}
	recordType := req.RecordType
	recordValueArr := marshalStringArray(req.RecordValue)

	err = s.tx.QueryRow(`
INSERT INTO assets(type, domain, root_domain, record_type, record_value, ip, c_segment, icp, company_id, company_source, task_ids)
VALUES ('subdomain', $1, $2, $3, $4::text[], $5, $6::cidr, $7, $8, 'scope', $9::bigint[])
ON CONFLICT (domain, COALESCE(record_type,'')) WHERE type = 'subdomain' DO UPDATE SET
    ip           = COALESCE(EXCLUDED.ip, assets.ip),
    c_segment    = COALESCE(EXCLUDED.c_segment, assets.c_segment),
    icp          = COALESCE(EXCLUDED.icp, assets.icp),
    company_id   = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN assets.company_id
        ELSE COALESCE(EXCLUDED.company_id, assets.company_id)
    END,
    company_source = CASE
        WHEN assets.company_source = 'explicit' AND assets.company_id IS NOT NULL THEN 'explicit'
        WHEN EXCLUDED.company_id IS NOT NULL THEN 'scope'
        ELSE assets.company_source
    END,
    task_ids     = (SELECT ARRAY(SELECT DISTINCT unnest(assets.task_ids || EXCLUDED.task_ids))),
    record_value = (SELECT ARRAY(SELECT DISTINCT unnest(assets.record_value || EXCLUDED.record_value))),
    extra        = assets.extra || EXCLUDED.extra,
    last_seen    = now()
RETURNING id`, domain, rootDomain, recordType, recordValueArr, ipVal, csegVal, icpVal, companyID, taskIDs).Scan(&id)
	return id, err
}
