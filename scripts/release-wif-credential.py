#!/usr/bin/python3
"""Docker credential protocol: projected JWT -> STS -> configured source -> Oberth role.

Only the credential-helper pipe receives credentials. No credentials are cached,
written to files, passed in arguments, or included in diagnostics.
"""
import json
import os
from pathlib import Path
import re
import ssl
import stat
import sys
import urllib.parse
import urllib.request

REGISTRY = 'europe-west4-docker.pkg.dev'
STS = 'https://sts.googleapis.com/v1/token'
ADC = '/run/oberth-gar/adc.json'
TOKEN = '/run/oberth-gar/token'
LIMIT = 65536

# Exact audience allowlist. The existing server projects the first-hop account
# from administrator-owned WIF configuration into a read-only ADC mount.
# Source account permissions are enforced by Google, not a client-side pairing.
ROLES = {
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-images/providers/oberth': 'image-writer',
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-reader/providers/oberth': 'image-reader',
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-charts/providers/oberth': 'chart-writer',
}
SOURCE_ACCOUNT_ERROR = ('missing or invalid ADC service_account_impersonation_url; '
                        'configure argo.releaseWIF.roles.<role>.service_account')


class SourceAccountError(ValueError):
    """Safe configuration diagnostic, containing no supplied values."""


def source_iam(config):
    iam = config.get('service_account_impersonation_url')
    if not isinstance(iam, str) or not re.fullmatch(
            r'https://iamcredentials\.googleapis\.com/v1/projects/-/serviceAccounts/'
            r'[a-z][a-z0-9-]*@[a-z][a-z0-9-]*\.iam\.gserviceaccount\.com:generateAccessToken', iam):
        raise SourceAccountError(SOURCE_ACCOUNT_ERROR)
    return iam


# Oberth's repositories already grant each matching Oberth principal access,
# and each configured source principal TokenCreator on only that target. Keep
# this second hop fixed by the validated audience, never ADC/env input (#875).
OBERTH_ROLES = {
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-images/providers/oberth':
        'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/oberth-release-publisher@skipopsmain.iam.gserviceaccount.com:generateAccessToken',
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-reader/providers/oberth':
        'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/oberth-release-reader@skipopsmain.iam.gserviceaccount.com:generateAccessToken',
    '//iam.googleapis.com/projects/138624361369/locations/global/workloadIdentityPools/oberth-release-charts/providers/oberth':
        'https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/oberth-chart-publisher@skipopsmain.iam.gserviceaccount.com:generateAccessToken',
}


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate field')
        result[key] = value
    return result


def decode(raw):
    if not raw or len(raw) > LIMIT:
        raise ValueError('response size')
    value = json.loads(raw, object_pairs_hook=unique)
    if not isinstance(value, dict):
        raise ValueError('object required')
    return value


def read(path):
    with Path(path).open('rb') as source:
        data = source.read(LIMIT + 1)
    if not data or len(data) > LIMIT:
        raise ValueError('input size')
    return data


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError('redirect refused')


def post(url, data, headers):
    context = ssl.create_default_context()
    context.minimum_version = ssl.TLSVersion.TLSv1_3
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), NoRedirect(),
        urllib.request.HTTPSHandler(context=context))
    request = urllib.request.Request(url, data=data, headers=headers, method='POST')
    with opener.open(request, timeout=20) as response:
        if response.status != 200:
            raise ValueError('exchange failed')
        return decode(response.read(LIMIT + 1))


def token(value):
    if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9._~+/=-]{1,16384}', value):
        raise ValueError('invalid token')
    return value


def credential(server):
    if server != REGISTRY or os.environ.get('OBERTH_RELEASE_WIF') != '1':
        raise ValueError('unapproved request')
    if os.environ.get('GOOGLE_APPLICATION_CREDENTIALS') != ADC:
        raise ValueError('unapproved ADC path')
    config = decode(read(ADC))
    audience = config.get('audience')
    if audience not in ROLES:
        raise ValueError('unapproved audience')
    iam = source_iam(config)
    expected = {
        'type': 'external_account', 'audience': audience,
        'subject_token_type': 'urn:ietf:params:oauth:token-type:jwt',
        'token_url': STS, 'service_account_impersonation_url': iam,
        'credential_source': {'file': TOKEN},
    }
    if config != expected:
        raise ValueError('unapproved ADC configuration')
    subject = read(TOKEN).decode('ascii')
    if not re.fullmatch(r'[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+', subject):
        raise ValueError('invalid projected token')
    # Google verifies signature, issuer, audience and provider conditions.
    exchanged = post(STS, urllib.parse.urlencode({
        'audience': audience,
        'grant_type': 'urn:ietf:params:oauth:grant-type:token-exchange',
        'requested_token_type': 'urn:ietf:params:oauth:token-type:access_token',
        'subject_token_type': config['subject_token_type'], 'subject_token': subject,
        'scope': 'https://www.googleapis.com/auth/cloud-platform',
    }).encode('ascii'), {'Content-Type': 'application/x-www-form-urlencoded'})
    if exchanged.get('token_type') != 'Bearer':
        raise ValueError('invalid token type')
    federated = token(exchanged.get('access_token'))
    impersonated = post(iam, json.dumps({
        'scope': ['https://www.googleapis.com/auth/cloud-platform'], 'lifetime': '600s',
    }).encode('ascii'), {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + federated})
    source = token(impersonated.get('accessToken'))
    delegated = post(OBERTH_ROLES[audience], json.dumps({
        'scope': ['https://www.googleapis.com/auth/cloud-platform'], 'lifetime': '600s',
    }).encode('ascii'), {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + source})
    return {'Username': 'oauth2accesstoken', 'Secret': token(delegated.get('accessToken'))}


def main():
    try:
        if sys.argv[1:] != ['get']:
            raise ValueError('unsupported operation')
        if not stat.S_ISFIFO(os.fstat(1).st_mode):
            raise ValueError('credential pipe required')
        server = sys.stdin.readline(1024).rstrip('\n')
        # This stdout is the private Docker helper protocol pipe, never a log.
        result = credential(server)
        sys.stdout.write(json.dumps(result) + '\n')
    except SourceAccountError:
        sys.stderr.write(SOURCE_ACCOUNT_ERROR + '\n')
        return 1
    except Exception:
        sys.stderr.write('keyless registry authentication failed\n')
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
