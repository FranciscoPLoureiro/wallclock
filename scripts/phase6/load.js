// Sustained load on the ticket office, long enough to profile properly.
//
// The ticket office ships two generators and neither fits this experiment.
// campaign.js is a single simultaneous burst, which is the midnight moment it
// exists to model and is over in a second. ramp.js holds pressure for a thirty
// second plateau, which is half of one profiling window short of useful here:
// the protocol is seven consecutive twenty-five second windows, so the system
// has to be in the same steady state for the better part of four minutes.
//
// Everything else is theirs -- same endpoint, same headers, same per-iteration
// identity so that repeated iterations measure contention rather than the
// duplicate check. Only the shape of the load is different, and it is
// different for a reason that belongs to this repository rather than to
// theirs.
import http from 'k6/http';
import { Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://api:8080';
const VUS = Number(__ENV.VUS || 100);
const PLATEAU = __ENV.PLATEAU || '200s';

function uuid() {
  const hex = '0123456789abcdef';
  let out = '';
  for (let i = 0; i < 36; i++) {
    if (i === 8 || i === 13 || i === 18 || i === 23) out += '-';
    else if (i === 14) out += '4';
    else if (i === 19) out += hex[(Math.random() * 4) | 8];
    else out += hex[(Math.random() * 16) | 0];
  }
  return out;
}

// Counted apart, and this is not tidiness. A single "refused" counter made a
// run of ten million rate-limited 429s look exactly like a run that exercised
// the purchase path, and the first version of this experiment measured the
// rate limiter for three and a half minutes without saying so.
const sold = new Counter('tickets_sold');
const soldOut = new Counter('refused_stock_exhausted');
const rateLimited = new Counter('refused_rate_limited');
const duplicate = new Counter('refused_already_purchased');
const serverErrors = new Counter('server_errors');

export const options = {
  // p(99) is not in k6's default trend statistics, and the question this
  // experiment came from is stated at the p99: 153 ms to 291 ms.
  summaryTrendStats: ['avg', 'min', 'med', 'p(95)', 'p(99)', 'max'],
  scenarios: {
    plateau: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '5s', target: VUS },
        { duration: PLATEAU, target: VUS },
        { duration: '5s', target: 0 },
      ],
      gracefulRampDown: '5s',
    },
  },
  // No pass/fail thresholds. This script is an instrument for an experiment
  // about where time goes, not a gate: a threshold here would turn a run that
  // answered the question into a red summary because the system was slower
  // under one of the two conditions -- which is the finding, not a failure.
  // The counts below are still recorded, because a run that sold nothing was
  // measuring an idle stack and has to be recognisable afterwards.
};

export default function () {
  const res = http.post(`${BASE_URL}/api/v1/tickets/purchase`, null, {
    headers: {
      'X-User-ID': `student-${__VU}-${__ITER}`,
      'Idempotency-Key': uuid(),
    },
  });
  if (res.status === 202) {
    sold.add(1);
  } else if (res.status === 429) {
    rateLimited.add(1);
  } else if (res.status === 409) {
    const code = res.json('error.code');
    if (code === 'already_purchased') duplicate.add(1);
    else soldOut.add(1);
  } else if (res.status >= 500) {
    serverErrors.add(1);
  }
}
