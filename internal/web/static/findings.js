// The project's own account of itself: what it is, what was measured, and
// what the measurements said. Kept beside the instrument deliberately —
// the numbers in the explorer are the same numbers argued about here.
window.FINDINGS_HTML = `
<div>
  <h1>Routing messages by what they mean</h1>
  <p class="lede">Conventional pub/sub routes by exact topic match: pick a topic, receive
  everything published to it, filter the rest yourself. This replaces that with a
  judgment made per message — subscribers say what they care about in plain language,
  and the broker decides.</p>

  <h2>The idea</h2>
  <p>A subscriber states an interest: <em>&ldquo;database and storage problems, including
  disk capacity&rdquo;</em>. A message arrives. Rather than matching strings, the broker asks
  whether this message should be <strong>delivered</strong> to someone who wants that —
  and delivers only if the answer is yes.</p>
  <p>The distinction between <em>delivered</em> and <em>about</em> carries more weight than it
  looks. A routine deploy notice naming <code>checkout-api</code> is topically related to a
  subscriber watching for checkout outages, and they do not want it. Asking about delivery
  gets that right; asking about similarity does not.</p>

  <h2>Why this is affordable</h2>
  <p>Calling a language model once per message per subscriber should be ruinous.
  <a href="https://docs.typesafe.ai">Jev</a> is a System One model: it returns typed answers
  with calibrated probabilities instead of generated text, ingests the state once and
  evaluates every question against it in parallel, and bills only input tokens.</p>
  <p>So one request carries the message plus every subscriber's predicate. Measured here:</p>
  <div class="figure">
    <table>
      <thead><tr><th>subscribers</th><th>latency</th><th>tokens/req</th><th>$/1k publishes</th></tr></thead>
      <tbody>
        <tr><td>6</td><td>202 ms</td><td>875</td><td>$0.037</td></tr>
        <tr><td>25</td><td>191 ms</td><td>2,599</td><td>$0.109</td></tr>
        <tr><td>50</td><td>230 ms</td><td>4,985</td><td>$0.209</td></tr>
        <tr><td>100</td><td>221 ms</td><td>9,771</td><td>$0.410</td></tr>
        <tr><td>200</td><td>327 ms</td><td>19,355</td><td>$0.813</td></tr>
      </tbody>
    </table>
    <caption>Thirty-three times the work for 62% more wall time. Cost is linear;
    latency is very nearly not.</caption>
  </div>

  <h2>What was measured</h2>
  <p>The project is an experiment packaged as a library. Working code was never the
  deliverable on its own — the question was whether semantic routing is reliable enough
  to depend on. Three measurements, and &ldquo;it is not&rdquo; was always a permitted answer.</p>

  <div class="verdict">
    <span class="m">M1</span><span class="pass">stability &mdash; is the decision reproducible? <b>pass</b></span>
    <span class="m">M2</span><span class="pass">batch degradation &mdash; do answers worsen at scale? <b>pass</b></span>
    <span class="m">M3</span><span class="fail">wording &mdash; does phrasing decide the outcome? fail</span>
  </div>

  <h3>M1 &mdash; stability</h3>
  <p>Twenty repeats of twenty message/interest pairs. <strong>Flip rate 0.0%</strong>: not one
  pair was non-unanimous. Standard deviation never exceeded 0.0168, and seven pairs returned
  identical probabilities on all twenty runs.</p>
  <p>Re-analysed at thresholds from 0.2 to 0.9, the flip rate stays at zero. No case straddles
  any threshold, so where the line sits changes <em>which</em> messages are delivered but never
  whether the decision is stable.</p>
  <p>The near miss held. That deploy notice naming <code>checkout-api</code> scored
  <span class="num">0.040</span> against a subscriber watching for checkout outages.</p>

  <h3>M2 &mdash; batch degradation</h3>
  <p>Anchor cases held fixed while the request was padded with filler subscribers, from 6 to 200.
  <strong>Zero decision changes.</strong> The largest drift across a thirty-three-fold change in
  batch size was 0.0140 &mdash; smaller than the run-to-run variance of asking the same question
  twice. Adding 194 subscribers to a request moves an answer less than repetition does.</p>

  <h3>M3 &mdash; wording</h3>
  <p>Three intents, each written five ways, judged in the same request so they differ only in
  phrasing. <strong>23.8% of intents routed differently</strong> depending purely on wording,
  and wording moved answers <strong>27.9&times;</strong> as much as repetition did.</p>
  <div class="figure">
    <table>
      <thead><tr><th>phrasing of &ldquo;storage problems&rdquo;</th><th>score</th><th></th></tr></thead>
      <tbody>
        <tr><td>database and storage problems, including disk capacity</td><td>0.812</td><td>deliver</td></tr>
        <tr><td>storage trouble, for example a disk near capacity</td><td>0.725</td><td>deliver</td></tr>
        <tr><td>anything where persistent storage is failing or filling up&hellip;</td><td>0.282</td><td>skip</td></tr>
        <tr><td>issues with disks or databases running out of room</td><td>0.179</td><td>skip</td></tr>
        <tr><td>disk problems</td><td>0.144</td><td>skip</td></tr>
      </tbody>
    </table>
    <caption>One message &mdash; a database failover failure &mdash; against five ways of saying
    the same thing. Same intent, opposite outcome.</caption>
  </div>
  <p>The pattern is consistent. Normalising each case so 1.0 is its most inclusive wording:
  examples 0.77, terse 0.71, original 0.69, verbose 0.44, plain language 0.38,
  <strong>with an exclusion clause 0.19</strong>. The more you specify, the narrower it gets,
  and exclusion clauses are by far the strongest lever &mdash; stronger than the threshold.</p>

  <h2>What the failure means</h2>
  <div class="pull"><p>The system reliably delivers what you asked for. The hard part is
  knowing what you asked for.</p></div>
  <p>The model is not being erratic &mdash; M1 and M2 established it is highly consistent. It
  reads each predicate literally, and by that standard it is arguably right: a failover failure
  genuinely is not a &ldquo;disk problem&rdquo;. The flawed assumption is the user's, that
  paraphrases of an intent are interchangeable.</p>
  <p>So this is a <strong>usability</strong> failure rather than a reliability one, and it is the
  one that would bite in practice. An operator's problem stops being <em>what do I want?</em> and
  becomes <em>what phrasing gets me what I want?</em></p>
  <p>Which is what the live view beside this page is for: register an interest, publish
  something, and see what it actually catches before trusting it.</p>

  <h2>How it is built</h2>
  <p>The broker is <a href="https://github.com/damiensmith1/go-ws-server">go-ws-server</a>, a
  separate project consumed as a tagged dependency and never forked. It is generic and knows
  nothing about semantics. This project supplies four of its extension points: a custom
  <code>interest</code> verb, a candidate source backed by Redis, the judge itself, and
  connection lifecycle hooks that drop a subscriber's interests when it disconnects.</p>
  <p>Judging happens once, at publish, and the decision travels with the message, so every
  instance and every replay agree. If Jev is unavailable, routing falls back to ordinary topic
  delivery rather than dropping messages &mdash; a broker that silently stops delivering when
  its classifier is down fails worse than one that briefly over-delivers.</p>
  <p>Total spend across all three experiments: <span class="num">$0.064</span>.</p>
</div>`;
