"""Exact existing browser OAuth correction through installed official ak shell.

Installation identity/host values are external Ansible variables. No secrets are
read or created. This helper never creates resources or touches other providers.
"""
import json
from copy import deepcopy
from django.db import transaction
from authentik.core.models import Application, User
from authentik.providers.proxy.models import ProxyProvider
from authentik.providers.oauth2.models import ScopeMapping, RedirectURI, RedirectURIMatchingMode
from authentik.policies.models import PolicyBinding
from authentik.policies.engine import PolicyEngine
from authentik.outposts.models import Outpost

required = {'provider_id', 'provider_name', 'app_slug', 'owner_id',
            'owner_username', 'outpost_id', 'external_host', 'callback'}
assert set(APPROVED_BROWSER) == required, 'Exact approved external settings required'
c = APPROVED_BROWSER
assert c['external_host'].startswith('https://')
assert c['callback'] == c['external_host'].rstrip('/') + '/outpost.goauthentik.io/callback?X-authentik-auth-callback=true'
managed = {
    'goauthentik.io/providers/oauth2/scope-openid': 'openid',
    'goauthentik.io/providers/oauth2/scope-profile': 'profile',
    'goauthentik.io/providers/oauth2/scope-email': 'email',
    'goauthentik.io/providers/proxy/scope-proxy': 'ak_proxy',
}
def public_snapshot(provider):
    return (provider.name, provider.mode, provider.external_host,
            provider.internal_host, provider.skip_path_regex,
            provider.client_id, provider.client_type, provider.grant_types,
            provider._redirect_uris, provider.signing_key_id,
            provider.include_claims_in_id_token,
            tuple(sorted(str(x) for x in provider.property_mappings.values_list('pk', flat=True))))

with transaction.atomic():
    provider = ProxyProvider.objects.select_for_update().defer('client_secret', 'cookie_secret').get(pk=c['provider_id'], name=c['provider_name'])
    app = Application.objects.select_for_update().get(slug=c['app_slug'], provider=provider)
    owner = User.objects.get(pk=c['owner_id'], username=c['owner_username'])
    outpost = Outpost.objects.select_for_update().get(pk=c['outpost_id'])
    assert provider.mode == 'forward_single' and provider.external_host == c['external_host'] and provider.skip_path_regex == ''
    assert provider.client_type == 'confidential' and app.policy_engine_mode == 'all'
    bindings_before = list(PolicyBinding.objects.filter(target=app).values('pk', 'user_id', 'group_id', 'policy_id', 'enabled', 'negate', 'failure_result', 'order'))
    assert len(bindings_before) == 1
    binding = bindings_before[0]
    assert binding['user_id'] == owner.pk and binding['group_id'] is None and binding['policy_id'] is None and binding['enabled'] and not binding['negate'] and not binding['failure_result']
    members_before = set(outpost.providers.values_list('pk', flat=True))
    assert provider.pk in members_before
    config_before = deepcopy(outpost._config)
    audience_before = provider.client_id
    others_before = {p.pk: public_snapshot(p) for p in ProxyProvider.objects.exclude(pk=provider.pk).defer('client_secret', 'cookie_secret')}
    scopes = list(ScopeMapping.objects.filter(managed__in=managed))
    assert len(scopes) == 4 and {s.managed: s.scope_name for s in scopes} == managed
    desired_redirects = [RedirectURI(RedirectURIMatchingMode.STRICT, c['callback'])]
    desired_grants = ['authorization_code']
    desired_scopes = {s.pk for s in scopes}
    changed = (provider.redirect_uris != desired_redirects or provider.grant_types != desired_grants or set(provider.property_mappings.values_list('pk', flat=True)) != desired_scopes)
    if changed:
        # Preserve initialized client identity, signing settings and flows.
        # Do not call broad defaults (which also enable password/client_credentials).
        provider.redirect_uris = desired_redirects
        provider.grant_types = desired_grants
        provider.save(update_fields=['_redirect_uris', 'grant_types'])
        provider.property_mappings.set(scopes)
    provider.refresh_from_db(fields=['_redirect_uris', 'grant_types', 'client_id'])
    assert provider.redirect_uris == desired_redirects and provider.grant_types == desired_grants
    assert set(provider.property_mappings.values_list('pk', flat=True)) == desired_scopes
    assert provider.client_id == audience_before
    assert list(PolicyBinding.objects.filter(target=app).values('pk', 'user_id', 'group_id', 'policy_id', 'enabled', 'negate', 'failure_result', 'order')) == bindings_before
    assert {p.pk: public_snapshot(p) for p in ProxyProvider.objects.exclude(pk=provider.pk).defer('client_secret', 'cookie_secret')} == others_before
    outpost.refresh_from_db()
    assert outpost._config == config_before and set(outpost.providers.values_list('pk', flat=True)) == members_before
    # Evaluate the sole owner and two distinct existing non-owner users. No login.
    nonowners = list(User.objects.exclude(pk=owner.pk).order_by('pk')[:2])
    assert len(nonowners) == 2
    outcomes = {str(u.pk): PolicyEngine(app, u).build().result.passing for u in [owner] + nonowners}
    assert outcomes[str(owner.pk)] and not any(outcomes[str(u.pk)] for u in nonowners)
    print('BROWSER_RESULT:' + json.dumps({'changed': changed, 'provider': provider.pk,
        'callback_count': 1, 'callback_strict': True, 'grants': desired_grants,
        'scopes': sorted(managed.values()), 'owner_id': owner.pk,
        'policy_tests': outcomes, 'other_providers_unchanged': True,
        'outpost_unchanged': True, 'audience_unchanged': True}))
