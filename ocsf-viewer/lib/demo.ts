import { normalize, type JsonObject } from './events';
// Deterministic, fictional incident, using documentation-only IP ranges.
export function demoEvents() {
  const end = Date.UTC(2026, 8, 9, 14, 32, 18);
  const classes = [4001, 3002, 4003, 1007, 2004, 6003];
  const sources = [
    'VPC Flow Logs',
    'Okta',
    'Route 53',
    'Falcon',
    'GuardDuty',
    'CloudTrail',
  ];
  const users = [
    'alex.morgan',
    'svc-deploy',
    'jordan.lee',
    'svc-backup',
    'sam.chen',
    'riley.park',
  ];
  const hosts = [
    'api-prod-03',
    'worker-prod-12',
    'db-primary-01',
    'runner-ci-04',
    'gateway-02',
    'web-prod-08',
  ];
  return Array.from({ length: 1248 }, (_, i) => {
    const kind = i % 6;
    const suspicious = i < 24 || i % 43 === 0;
    const severity = suspicious
      ? i % 5 === 0
        ? 5
        : 4
      : i % 11 === 0
        ? 3
        : i % 4 === 0
          ? 2
          : 1;
    const classId = classes[kind];
    const activityId = kind === 0 ? 6 : kind === 2 ? 1 : 1;
    const user = users[(i * 7) % users.length];
    const host = hosts[(i * 13) % hosts.length];
    const descriptions = suspicious
      ? [
          'Outbound connection to an unusual destination',
          'Repeated authentication failures for a privileged account',
          'DNS query to a newly observed domain',
          'Shell process launched by an unexpected parent',
          'Possible credential misuse on production workload',
          'Unusual API access to a production secret',
        ]
      : [
          'Network traffic recorded',
          'User session authenticated',
          'DNS query resolved',
          'Process started',
          'Security finding updated',
          'Cloud API request completed',
        ];
    const raw: JsonObject = {
      time: end - i * 6901 - (i % 7) * 313,
      class_uid: classId,
      category_uid: Math.floor(classId / 1000),
      activity_id: activityId,
      type_uid: classId * 100 + activityId,
      severity_id: severity,
      status_id: kind === 1 && suspicious ? 2 : 1,
      metadata: {
        version: '1.6.0',
        uid: `demo-${String(i + 1).padStart(6, '0')}`,
        product: {
          name: sources[kind],
          vendor_name: [
            'Amazon Web Services',
            'Okta',
            'Amazon Web Services',
            'CrowdStrike',
            'Amazon Web Services',
            'Amazon Web Services',
          ][kind],
        },
        log_name: 'production',
      },
      message: descriptions[kind],
      device: { hostname: host, uid: `host-${host}`, type_id: 1 },
    };
    if (kind === 0)
      Object.assign(raw, {
        src_endpoint: {
          ip: `10.24.${i % 8}.${(i % 200) + 10}`,
          port: 49152 + (i % 15000),
        },
        dst_endpoint: {
          ip: suspicious ? '203.0.113.42' : '192.0.2.80',
          port: 443,
        },
        connection_info: {
          protocol_name: 'TCP',
          protocol_num: 6,
          direction_id: 2,
        },
        traffic: { bytes: 8300 + i * 137, packets: 12 + (i % 90) },
      });
    if (kind === 1)
      Object.assign(raw, {
        user: { name: user, uid: `user-${i % 6}`, type_id: 1 },
        src_endpoint: {
          ip: suspicious ? '198.51.100.24' : `10.24.1.${i % 200}`,
        },
        auth_protocol_id: 6,
        is_mfa: !suspicious,
        logon_type_id: 3,
      });
    if (kind === 2)
      Object.assign(raw, {
        query: {
          hostname: suspicious ? 'telemetry.example.net' : 'api.example.com',
          type: 'A',
          class: 'IN',
        },
        src_endpoint: { ip: `10.24.2.${i % 200}` },
        dst_endpoint: { ip: '192.0.2.53', port: 53 },
        rcode_id: 0,
      });
    if (kind === 3)
      Object.assign(raw, {
        actor: {
          user: { name: user },
          process: { name: suspicious ? 'python3' : 'systemd', pid: 1900 + i },
        },
        process: {
          name: suspicious ? 'bash' : 'node',
          pid: 2400 + i,
          cmd_line: suspicious
            ? 'bash -c curl https://telemetry.example.net/health'
            : 'node /app/server.js',
        },
      });
    if (kind === 4)
      Object.assign(raw, {
        finding_info: {
          uid: `finding-${i}`,
          title: descriptions[kind],
          desc: 'Multiple signals associated with the same production workload.',
          analytic: { name: 'Credential anomaly', type_id: 1 },
          attacks: [
            {
              technique: { uid: 'T1078', name: 'Valid Accounts' },
              tactic: { uid: 'TA0001', name: 'Initial Access' },
            },
          ],
        },
        resources: [{ name: host, uid: `instance-${i}`, type: 'EC2 Instance' }],
        confidence_id: suspicious ? 3 : 1,
      });
    if (kind === 5)
      Object.assign(raw, {
        actor: { user: { name: user, uid: `user-${i % 6}` } },
        api: {
          operation: suspicious ? 'GetSecretValue' : 'DescribeInstances',
          service: {
            name: suspicious
              ? 'secretsmanager.amazonaws.com'
              : 'ec2.amazonaws.com',
          },
        },
        src_endpoint: { ip: '198.51.100.24' },
        resources: [{ name: suspicious ? 'prod/database/credentials' : host }],
      });
    return normalize(raw, `demo:${i}`);
  });
}
