// service.go はよく使われるポート → サービス名（recon のリスニングポートを service アセットとして
// 登録する際の命名に使う。意味は PivotHub の recon.py の PORT_SERVICES と同じで、イントラネットで
// 頻出するポートのみ保持）。純粋関数。
package recon

// portServices はよく使われるポート → 標準サービス名。
var portServices = map[int]string{
	21: "FTP", 22: "SSH", 23: "Telnet", 25: "SMTP", 53: "DNS",
	80: "HTTP", 88: "Kerberos", 110: "POP3", 111: "rpcbind", 135: "MSRPC",
	139: "NetBIOS-SSN", 143: "IMAP", 389: "LDAP", 443: "HTTPS", 445: "SMB",
	465: "SMTPS", 514: "Syslog", 587: "SMTP-Submission", 636: "LDAPS",
	873: "rsync", 993: "IMAPS", 995: "POP3S", 1080: "SOCKS", 1433: "MSSQL",
	1521: "Oracle", 2049: "NFS", 2375: "Docker", 3306: "MySQL", 3389: "RDP",
	5432: "PostgreSQL", 5900: "VNC", 5985: "WinRM-HTTP", 5986: "WinRM-HTTPS",
	6379: "Redis", 7001: "WebLogic", 8009: "AJP", 8080: "HTTP-Proxy",
	8443: "HTTPS-Alt", 9200: "Elasticsearch", 11211: "Memcached",
	27017: "MongoDB",
}

// ServiceOf はポートに対応する標準サービス名を返す。未収録の場合は "" を返す。
func ServiceOf(port int) string { return portServices[port] }
