/**
 * GovernancePage is a placeholder. Budgets and pause/resume/terminate are
 * Phase 5b: they need engine enforcement that does not exist yet. This page is
 * honest about that. It does not fake a budget cap or a working pause control.
 * The one control that is real today is CancelRun, surfaced on the run detail
 * page, so it is linked rather than duplicated here.
 */
export function GovernancePage() {
  return (
    <div className="space-y-4">
      <h1 className="text-xl font-semibold">Governance</h1>

      <div className="rounded border border-amber-300 bg-amber-50 p-3 text-sm text-amber-900">
        <p className="font-semibold">Phase 5b, not enforced yet</p>
        <p>
          Budgets and pause controls need engine support that ships in Phase 5b. The controls below
          are inert previews. They do not change any server behavior, and the server enforces no
          spend cap today.
        </p>
      </div>

      <section className="rounded border border-gray-200 bg-white p-4">
        <h2 className="font-medium">Workflow budget (preview)</h2>
        <p className="mt-1 text-sm text-gray-500">
          When this lands, a budget will be a soft ceiling on estimated runner spend per period, not
          a hard billing cap. Estimated cost can drift from a real invoice.
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
        <h2 className="font-medium">Control actions (preview)</h2>
        <p className="mt-1 text-sm text-gray-500">
          Pause and resume are Phase 5b and require the admin scope. Terminate maps to CancelRun,
          which works today from a run's detail page.
        </p>
      </section>
    </div>
  );
}
