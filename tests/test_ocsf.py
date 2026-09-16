from copy import deepcopy
import json
from pathlib import Path
import unittest

from warden.ocsf import Exporter


def native(kind='http.request.external', **fields):
    return {'schema_version':'1.0.0','event_id':'00000000-0000-4000-8000-000000000001',
            'time':'2026-09-09T22:37:32.797Z','event_type':kind,'severity':'info',
            'producer':{'name':'warden','version':'0.1.0','instance_id':'instance-1'},
            'sequence':1,'previous_hash':'0'*64,'event_hash':'a'*64,**fields}


class OCSFTests(unittest.TestCase):
    def test_http_projection_and_response_correlation(self):
        exporter=Exporter()
        source=native(request_id='r',request={'method':'GET','host':'www.google.com','path':'/',
                                             'body':{'bytes':0,'capture':'omitted_policy'}})
        before=deepcopy(source); event=exporter.convert(source)
        self.assertEqual(source,before)
        self.assertEqual(event['time'],1788993452797)
        self.assertEqual(event['type_uid'],400203)
        self.assertEqual(event['metadata']['uid'],source['event_id'])
        self.assertEqual(event['unmapped']['warden'],source)
        response=exporter.convert(native('http.response',request_id='r',status=200,response={'body':{'bytes':5000000,'capture':'omitted_policy'}}))
        self.assertEqual(response['dst_endpoint']['hostname'],'www.google.com')
        self.assertEqual(response['http_response'],{'code':200,'body_length':5000000})
        self.assertEqual(response['status_id'],1)
        self.assertEqual(response['metadata']['correlation_uid'],'r')

    def test_body_omission_even_for_legacy_input(self):
        event=Exporter().convert(native('http.response',status=200,response={'body':{'bytes':50,'content':'PRIVATE BODY','capture':'complete_redacted'}}))
        self.assertNotIn('PRIVATE BODY',json.dumps(event))
        self.assertIn('body_retention',event['unmapped'])

    def test_dns_firewall_and_control_fields(self):
        exporter=Exporter()
        dns=exporter.convert(native('dns.query',hostname='example.com',reason='DNS A',destination={'client':'10.77.0.73','resolver':'10.77.0.1'}))
        self.assertEqual(dns['type_uid'],400301)
        self.assertEqual(dns['query'],{'hostname':'example.com','type':'A'})
        denied=exporter.convert(native('network.denied',destination={'SRC':'10.77.0.73','DST':'140.82.116.4','DPT':'22','PROTO':'TCP'}))
        self.assertEqual(denied['dst_endpoint']['port'],22)
        self.assertEqual(denied['status_id'],2)
        grant=exporter.convert(native('approval.granted',request_id='r',grant_id='g',actor={'type':'human'}))
        self.assertEqual(grant['type_uid'],600303)
        self.assertEqual(grant['unmapped']['warden']['grant_id'],'g')

    def test_unknowns_remain_inspectable_and_invalid_versions_fail(self):
        event=Exporter().convert(native('future.event'))
        self.assertEqual(event['type_uid'],600399)
        with self.assertRaises(ValueError): Exporter().convert(native(schema_version='2.0.0'))

    def test_correlation_is_bounded_and_does_not_cross_instances(self):
        exporter=Exporter(cache_size=1)
        exporter.convert(native(request_id='r',request={'method':'GET','host':'example.com','path':'/'}))
        other=native('http.response',request_id='r',status=200)
        other['producer']['instance_id']='instance-2'
        self.assertNotIn('dst_endpoint',exporter.convert(other))
        exporter.convert(native(request_id='s',request={'method':'GET','host':'other.example','path':'/'}))
        self.assertEqual(len(exporter.requests),1)

    def test_all_event_types_match_viewer_classification(self):
        path=Path(__file__).resolve().parents[1]/'ocsf-viewer/lib/catalog.json'
        if not path.exists(): self.skipTest('sibling viewer not included in standalone distribution')
        catalog=json.loads(path.read_text())
        kinds=json.loads((Path(__file__).resolve().parents[1]/'schemas/audit-event.schema.json').read_text())['properties']['event_type']['enum']
        for kind in kinds:
            event=Exporter().convert(native(kind)); cls=catalog['classes'][str(event['class_uid'])]
            self.assertEqual(event['metadata']['version'],catalog['version'])
            self.assertEqual(event['category_uid'],cls['category_uid'])
            self.assertIn(str(event['activity_id']),cls['attributes']['activity_id']['enum'])
