import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('portfolio_proxy', Path(__file__).parents[1] / 'deploy/hyperv/portfolio_proxy.py')
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)


class PortfolioProxyTests(unittest.TestCase):
    def test_only_monitoring_reads_and_grafana_queries_are_forwarded(self):
        for method, path in [('GET', '/monitor/d/wms-operations'), ('GET', '/monitor/public/app.js'),
                             ('POST', '/monitor/api/ds/query')]:
            self.assertTrue(proxy.allowed(method, path))
        for method, path in [('GET', '/auth/'), ('GET', '/etc/wms/secrets.json'),
                             ('POST', '/monitor/api/admin/users'), ('DELETE', '/monitor/api/dashboards/uid/x'),
                             ('GET', '/monitor/../auth/'), ('GET', '/monitor/%2e%2e/auth/'),
                             ('GET', '//remote.example/monitor/'), ('POST', '/monitor/api/ds/query?redirect=1')]:
            self.assertFalse(proxy.allowed(method, path), (method, path))

    def test_client_credentials_never_override_upstream_viewer_and_rewrite_only_origin(self):
        headers = proxy.upstream_headers({'Authorization': 'client-value', 'Cookie': 'private',
                                          'Host': 'untrusted', 'Content-Type': 'application/json'}, 'viewer', '192.168.1.4')
        self.assertEqual(headers['Authorization'], 'Basic viewer')
        self.assertEqual(headers['Host'], '192.168.1.4')
        self.assertNotIn('Cookie', headers)
        self.assertNotIn('client-value', str(headers))
        body = b'{"appUrl":"https://192.168.1.4/monitor/","up":1}'
        self.assertEqual(proxy.rewrite(body, '192.168.1.4', 18081),
                         b'{"appUrl":"http://127.0.0.1:18081/monitor/","up":1}')


if __name__ == '__main__':
    unittest.main()
