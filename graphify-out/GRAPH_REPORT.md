# Graph Report - .  (2026-04-25)

## Corpus Check
- 680 files · ~741,521 words
- Verdict: corpus is large enough that graph structure adds value.

## Summary
- 37 nodes · 59 edges · 10 communities detected
- Extraction: 100% EXTRACTED · 0% INFERRED · 0% AMBIGUOUS
- Token cost: 0 input · 0 output

## God Nodes (most connected - your core abstractions)
1. `request()` - 10 edges
2. `fetchAccounts()` - 3 edges
3. `buildQueryString()` - 2 edges
4. `login()` - 2 edges
5. `detectDuplicates()` - 2 edges
6. `deleteDuplicates()` - 2 edges
7. `batchHealthCheck()` - 2 edges
8. `testAccount()` - 2 edges
9. `deleteAccount()` - 2 edges
10. `bulkDeleteAccounts()` - 2 edges

## Surprising Connections (you probably didn't know these)
- `fetchAccounts()` --calls--> `request()`  [EXTRACTED]
  temp/apitest/frontend/src/api.ts → temp/apitest/frontend/src/api.ts  _Bridges community 0 → community 5_

## Communities

### Community 0 - "Community 0"
Cohesion: 0.38
Nodes (9): batchHealthCheck(), bulkDeleteAccounts(), deleteAccount(), deleteDuplicates(), detectDuplicates(), login(), logout(), request() (+1 more)

### Community 1 - "Community 1"
Cohesion: 0.22
Nodes (0): 

### Community 2 - "Community 2"
Cohesion: 0.67
Nodes (0): 

### Community 3 - "Community 3"
Cohesion: 0.67
Nodes (0): 

### Community 4 - "Community 4"
Cohesion: 0.67
Nodes (0): 

### Community 5 - "Community 5"
Cohesion: 1.0
Nodes (2): buildQueryString(), fetchAccounts()

### Community 6 - "Community 6"
Cohesion: 1.0
Nodes (0): 

### Community 7 - "Community 7"
Cohesion: 1.0
Nodes (0): 

### Community 8 - "Community 8"
Cohesion: 1.0
Nodes (0): 

### Community 9 - "Community 9"
Cohesion: 1.0
Nodes (0): 

## Knowledge Gaps
- **Thin community `Community 5`** (2 nodes): `buildQueryString()`, `fetchAccounts()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 6`** (2 nodes): `LoginPanel.tsx`, `LoginPanel()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 7`** (2 nodes): `BulkDeleteModal.tsx`, `BulkDeleteModal()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 8`** (2 nodes): `useAccountsQuery.ts`, `useAccountsQuery()`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.
- **Thin community `Community 9`** (1 nodes): `vite.config.ts`
  Too small to be a meaningful cluster - may be noise or needs more connections extracted.

## Suggested Questions
_Questions this graph is uniquely positioned to answer:_

- **Why does `request()` connect `Community 0` to `Community 5`?**
  _High betweenness centrality (0.029) - this node is a cross-community bridge._
- **Why does `fetchAccounts()` connect `Community 5` to `Community 0`?**
  _High betweenness centrality (0.001) - this node is a cross-community bridge._