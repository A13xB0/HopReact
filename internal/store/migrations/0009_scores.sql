-- CoreScope's structural-importance measures, so the status board can pick
-- out the backbone.
--
-- Two separate numbers because they answer different questions. traffic_share
-- is the fraction of mesh traffic that actually transits a node; bridge_score
-- is betweenness centrality, or how many shortest paths between other pairs
-- of nodes run through it. A quiet chokepoint scores low on the first and
-- high on the second — and losing it still cuts the mesh in half.
ALTER TABLE targets ADD COLUMN bridge_score REAL NOT NULL DEFAULT 0;
ALTER TABLE targets ADD COLUMN traffic_share REAL NOT NULL DEFAULT 0;
