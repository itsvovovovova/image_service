import encoding from 'k6/encoding';
import { check, sleep } from 'k6';
import http from 'k6/http';
import { Trend, Rate } from 'k6/metrics';

const base = __ENV.BASE_URL || 'http://127.0.0.1:8007';
const png = encoding.b64decode(open('./fixture.base64').trim());
const completion = new Trend('image_completion_ms', true);
const success = new Rate('image_completed');

export const options = {
  scenarios: {
    images: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 3),
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 12,
      maxVUs: 60,
    },
  },
  thresholds: {
    'http_req_duration{operation:submit}': ['p(95)<1000'],
    image_completion_ms: ['p(95)<8000'],
    image_completed: ['rate>0.99'],
    dropped_iterations: ['count==0'],
  },
};

export function setup() {
  const body = JSON.stringify({ username: `load_${Date.now()}`, password: 'qa-load-password' });
  const params = { headers: { 'Content-Type': 'application/json' } };
  if (http.post(base + '/register', body, params).status !== 201) throw new Error('Registration failed');
  const login = http.post(base + '/login', body, params);
  if (login.status !== 200) throw new Error('Login failed');
  return login.json('token');
}

export default function (token) {
  const headers = { Authorization: 'Bearer ' + token };
  const start = Date.now();
  const submission = http.post(base + '/task?filter=Negative', png, {
    headers: { ...headers, 'Content-Type': 'image/png' },
    tags: { operation: 'submit' },
  });
  if (!check(submission, { accepted: (r) => r.status === 202 })) {
    success.add(false);
    return;
  }
  const location = submission.headers.Location;
  while (Date.now() - start < 15000) {
    const result = http.get(base + location, { headers, tags: { name: 'GET /status/:id' } });
    if (result.status !== 200 || result.json('status') === 'failed') break;
    if (result.json('status') === 'succeeded') {
      completion.add(Date.now() - start);
      success.add(true);
      return;
    }
    sleep(0.25);
  }
  success.add(false);
}
