import { useCallback, useMemo } from 'react';
import ReactFlow, {
  Background,
  Controls,
  type Connection,
  type EdgeChange,
  type NodeChange,
  type NodeTypes,
  applyEdgeChanges,
  applyNodeChanges,
} from 'reactflow';

import { ActionNodeView } from './ActionNodeView';
import { useBuilderStore } from './store';

/**
 * Canvas is the reactflow DAG: action nodes wired by depends_on edges. The
 * reactflow change helpers live here, not in the store, so the store stays
 * free of reactflow's runtime and a read-only page (the runner view) does not
 * pull the whole library into the initial bundle.
 */
export function Canvas() {
  const nodes = useBuilderStore((state) => state.nodes);
  const edges = useBuilderStore((state) => state.edges);
  const setNodes = useBuilderStore((state) => state.setNodes);
  const setEdges = useBuilderStore((state) => state.setEdges);
  const connect = useBuilderStore((state) => state.connect);
  const select = useBuilderStore((state) => state.select);

  const nodeTypes = useMemo<NodeTypes>(() => ({ action: ActionNodeView }), []);

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      setNodes(applyNodeChanges(changes, useBuilderStore.getState().nodes));
    },
    [setNodes],
  );

  const onEdgesChange = useCallback(
    (changes: EdgeChange[]) => {
      setEdges(applyEdgeChanges(changes, useBuilderStore.getState().edges));
    },
    [setEdges],
  );

  const onConnect = useCallback(
    (connection: Connection) => {
      if (!connection.source || !connection.target) {
        return;
      }
      // The store owns edge creation: it dedupes by endpoint id and rejects a
      // self-edge, then appends a reactflow-shaped edge.
      connect(connection.source, connection.target);
    },
    [connect],
  );

  return (
    <div className="h-[32rem] rounded border border-gray-200 bg-white" data-testid="canvas">
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onConnect={onConnect}
        onNodeClick={(_event, node) => select(node.id)}
        onPaneClick={() => select(null)}
        fitView
      >
        <Background />
        <Controls />
      </ReactFlow>
    </div>
  );
}
