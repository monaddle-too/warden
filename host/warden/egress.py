"""Exact destination policy. An allowed AI endpoint can receive source code."""
import re

HOST = re.compile(r'(?=.{1,253}$)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}')
REPO = re.compile(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+')
CATEGORIES = {'ai','dependencies','development','updates'}

def validate(value):
    if not isinstance(value,dict) or set(value)!={'mode','destinations'} or value['mode'] not in ('restricted','public'):raise ValueError('invalid egress policy')
    if not isinstance(value['destinations'],list) or len(value['destinations'])>256:raise ValueError('invalid destinations')
    seen=set()
    for rule in value['destinations']:
        if not isinstance(rule,dict) or set(rule)!={'host','methods','category'}:raise ValueError('invalid destination rule')
        if not isinstance(rule['host'],str) or not HOST.fullmatch(rule['host']) or rule['host'] in seen:raise ValueError('destinations must be unique exact DNS names')
        seen.add(rule['host'])
        if rule['category'] not in CATEGORIES:raise ValueError('invalid destination category')
        if not isinstance(rule['methods'],list) or not rule['methods'] or any(m not in ('GET','HEAD','POST','PUT','PATCH','DELETE','OPTIONS') for m in rule['methods']):raise ValueError('invalid destination methods')
    return value

def permits(policy,host,method,scheme='https',tls=False):
    if not isinstance(host,str) or not HOST.fullmatch(host):return False
    if policy['mode']=='public':return True
    for rule in policy['destinations']:
        if rule['host']==host:
            if tls:return rule['category']=='updates'
            return scheme=='https' and method in rule['methods']
    return False
