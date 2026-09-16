"""Metadata-only OCSF 1.6.0 projection for OCSF Explorer and its ingester.

The native audit remains authoritative. Export never reads request bodies from
the proxy or approval database. The bounded correlation cache stores metadata.
"""
from collections import OrderedDict
from datetime import datetime, timezone
import ipaddress

from .retention import without_bodies

VERSION = '1.6.0'
HTTP_ACTIVITIES = {'CONNECT':1,'DELETE':2,'GET':3,'HEAD':4,'OPTIONS':5,'POST':6,'PUT':7,'TRACE':8,'PATCH':9}
SEVERITIES = {'debug':1,'info':1,'warning':3,'error':4,'critical':5}


def endpoint(ip=None, port=None, hostname=None):
    result = {}
    if ip:
        try: result['ip'] = str(ipaddress.ip_address(ip))
        except ValueError: pass
    if port is not None:
        try:
            number = int(port)
            if 0 <= number <= 65535: result['port'] = number
        except (TypeError, ValueError): pass
    if hostname: result['hostname'] = hostname
    return result


class Exporter:
    def __init__(self, cache_size=10000):
        self.requests = OrderedDict()
        self.cache_size = cache_size

    def convert(self, source):
        if str(source.get('schema_version','')).split('.')[0] != '1':
            raise ValueError('unsupported Warden audit major version')
        event = without_bodies(source)
        kind = event['event_type']
        request_id = event.get('request_id')
        producer = event['producer']
        key = (producer['instance_id'], request_id)
        request = event.get('request') or {}
        if request and request_id:
            self.requests[key] = {k:request[k] for k in ('method','host','path','scheme','port','http_version','operation') if k in request}
            self.requests.move_to_end(key)
            while len(self.requests) > self.cache_size: self.requests.popitem(last=False)
        elif request_id:
            request = self.requests.get(key, {})
        method = request.get('method','')
        hostname = event.get('hostname') or request.get('host')
        code = event.get('status')
        class_id, activity = 6003, 99
        if kind.startswith('http.') or kind.startswith('request.') or kind.startswith('egress.') or (kind=='proxy.error' and (hostname or request)):
            class_id, activity = 4002, HTTP_ACTIVITIES.get(method,0 if not method else 99)
        elif kind=='network.denied': class_id, activity = 4001, 5
        elif kind=='tls.passthrough': class_id, activity = 4001, 1
        elif kind=='dns.query': class_id, activity = 4003, 1
        elif kind in ('system.started','proxy.started'): class_id, activity = 6002, 3
        elif kind=='approval.requested': activity = 1
        elif kind in ('approval.granted','approval.denied','approval.revoked','policy.updated','credential.configured'): activity = 3
        elif kind=='credential.cleared': activity = 4
        denied = kind in ('request.denied','request.interrupted','network.denied','egress.denied','network.disconnected','approval.denied','approval.revoked','approval.revoked_all') or (kind=='proxy.error' and type(code) is int)
        status = 0
        if kind=='http.response' and type(code) is int: status = 2 if code>=400 else 1
        elif denied or kind=='proxy.error': status = 2
        elif kind in ('request.allowed','approval.granted','policy.updated','credential.configured','credential.cleared','system.started','proxy.started'): status = 1
        stamp = datetime.fromisoformat(event['time'].replace('Z','+00:00'))
        if stamp.tzinfo is None: raise ValueError('audit time must include a timezone')
        delta = stamp.astimezone(timezone.utc) - datetime(1970,1,1,tzinfo=timezone.utc)
        millis = (delta.days*86400+delta.seconds)*1000 + delta.microseconds//1000
        result = {
            'time':millis, 'class_uid':class_id, 'category_uid':class_id//1000,
            'activity_id':activity, 'type_uid':class_id*100+activity,
            'severity_id':SEVERITIES.get(event.get('severity'),0), 'status_id':status,
            'metadata':{'version':VERSION,'uid':event['event_id'],'event_code':kind,
                'product':{'name':'Warden','vendor_name':'Warden','version':producer['version']},
                'log_name':'warden.audit','log_version':event['schema_version']},
            'message':kind + (': '+event['reason'] if event.get('reason') else ''),
            'unmapped':{'warden':event},
        }
        if request_id: result['metadata']['correlation_uid'] = request_id
        if source != event:
            result['unmapped']['body_retention'] = 'Legacy body content omitted during export; native hash refers to the original event.'
        if class_id==4002:
            http = {}
            if method: http['http_method'] = method
            if request_id: http['uid'] = request_id
            url = {k:request[k] for k in ('path','port','scheme') if request.get(k) is not None}
            if hostname: url['hostname'] = hostname
            if request.get('path'): http['url'] = url
            if request.get('http_version'): http['version'] = request['http_version'].removeprefix('HTTP/')
            if request.get('body',{}).get('bytes') is not None: http['body_length'] = request['body']['bytes']
            if http: result['http_request'] = http
            if type(code) is int:
                response = {'code':code}
                size = event.get('response',{}).get('body',{}).get('bytes')
                if size is not None: response['body_length'] = size
                result['http_response'] = response
            if hostname: result['dst_endpoint'] = endpoint(hostname=hostname,port=request.get('port'))
        elif class_id==4001:
            d = event.get('destination',{})
            for name, value in [('src_endpoint',endpoint(d.get('SRC'),d.get('SPT'))),('dst_endpoint',endpoint(d.get('DST'),d.get('DPT')))]:
                if value: result[name] = value
            if kind=='tls.passthrough' and hostname:
                result.setdefault('dst_endpoint', {})['hostname'] = hostname
            if d.get('PROTO') in ('TCP','UDP'): result['connection_info'] = {'protocol_num':6 if d['PROTO']=='TCP' else 17}
        elif class_id==4003:
            if hostname:
                result['query'] = {'hostname':hostname}
                qtype = event.get('reason','').removeprefix('DNS ')
                if qtype: result['query']['type'] = qtype
            d = event.get('destination',{})
            for name,value in [('src_endpoint',endpoint(d.get('client'))),('dst_endpoint',endpoint(d.get('resolver'),53))]:
                if value: result[name] = value
        elif class_id==6002:
            result['app'] = {'name':'Warden proxy' if kind=='proxy.started' else 'Warden','version':producer['version']}
        else:
            result['api'] = {'operation':kind}
            result['actor'] = {'app_name':'Warden host webapp' if event.get('actor',{}).get('type')=='human' else 'Warden control plane'}
            result['src_endpoint'] = {'svc_name':'warden-control'}
        return result
