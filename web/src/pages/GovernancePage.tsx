/**
 * GovernancePage describes the budget and pause rules the server enforces.
 * The page has no controls of its own yet, so it points at the CLI and the
 * gateway routes instead of showing buttons that would do nothing. CancelRun
 * already lives on the run detail page, so it is linked rather than duplicated.
 */
export function GovernancePage() {
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Governance</h1>

      <div className="rounded border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900">
        <p className="font-semibold">Enforced by the server, read-only here</p>
        <p>
          The server refuses new runs of a paused workflow and checks each workflow's budget before
          a run starts. This page does not change either one yet. Use <code>cairnos pause</code>,{' '}
          <code>cairnos resume</code>, and <code>cairnos budget</code>, or the{' '}
          <code>/v1/governance</code> routes with an admin key.
        </p>
      </div>

      <section className="rounded border border-gray-200 bg-white p-4">
        <h2 className="font-medium">Workflow budget</h2>
        <p className="mt-1 text-sm text-gray-500">
          A budget is a soft ceiling on estimated runner spend per period, not a hard billing cap.
          Estimated cost can drift from a real invoice. Set it in the workflow file under{' '}
          <code>[workflow.budget]</code>.
        </p>
        <dl className="mt-3 grid grid-cols-2 gap-2 text-sm">
          <dt className="text-gray-500">Period</dt>
          <dd>day / week / month</dd>
          <dt className="text-gray-500">Limit</dt>
          <dd>USD per period</dd>
          <dt className="text-gray-500">On exceed</dt>
          <dd>pause new runs, or warn and continue</dd>
        </dl>
      </section>

      <section className="rounded border border-gray-200 bg-white p-4">
        <h2 className="font-medium">Control actions</h2>
        <p className="mt-1 text-sm text-gray-500">
          Pause and resume write an audit entry, and over the API they require the admin scope.
          Terminate maps to CancelRun, which works from a run's detail page.
        </p>
      </section>
    </div>
  );
}
