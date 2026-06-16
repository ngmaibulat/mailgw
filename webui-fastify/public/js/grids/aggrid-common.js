// Shared AG Grid wiring for the log viewers. Exposes `window.LogGrid` so each
// per-grid script (connection/delivery/mails/lookups) just declares its URL and
// columns. Server-side paging, search and sort via AG Grid's Infinite Row Model
// (the only server-driven model in Community — the full SSRM is Enterprise):
//
// - Paging: each page -> one request (offset = startRow, limit = block size).
//   logservice returns the real `total`, used directly as the row count.
// - Search: AG Grid floating column filters -> the proxy's
//   { search:[{field,operator,value}], searchLogic } shape.
// - Sort: AG Grid sortModel -> { sort:[{field,direction}] } (logservice
//   whitelists the field and defaults to id DESC when unsorted).
window.LogGrid = (function () {
    "use strict";

    var PAGE_SIZE = 50;

    var TEXT_OPTS = ["contains", "equals", "startsWith", "endsWith"];
    var NUM_OPTS = [
        "equals",
        "greaterThan",
        "greaterThanOrEqual",
        "lessThan",
        "lessThanOrEqual",
        "inRange",
    ];

    function fmtDt(p) {
        return p.value ? String(p.value).slice(0, 19).replace("T", " ") : "";
    }

    // AG Grid filter `type` -> logservice operator. Unsupported types
    // (notEqual/notContains/blank/...) return null and are dropped; the column
    // filterOptions below only expose the mapped ones.
    function mapOp(type) {
        switch (type) {
            case "contains":
                return "contains";
            case "equals":
                return "=";
            case "startsWith":
                return "begins";
            case "endsWith":
                return "ends";
            case "lessThan":
                return "<";
            case "lessThanOrEqual":
                return "<=";
            case "greaterThan":
                return ">";
            case "greaterThanOrEqual":
                return ">=";
            case "inRange":
                return "between";
            default:
                return null;
        }
    }

    function buildSearch(filterModel) {
        var out = [];
        if (!filterModel) return out;
        Object.keys(filterModel).forEach(function (field) {
            var f = filterModel[field];
            var op = mapOp(f.type);
            if (!op) return;
            if (op === "between") {
                if (f.filter != null && f.filterTo != null) {
                    out.push({
                        field: field,
                        operator: "between",
                        value: [f.filter, f.filterTo],
                    });
                }
            } else if (f.filter != null && f.filter !== "") {
                out.push({ field: field, operator: op, value: f.filter });
            }
        });
        return out;
    }

    // ---- column helpers ----------------------------------------------------
    function text(field, header, opts) {
        return Object.assign(
            {
                headerName: header,
                field: field,
                filter: "agTextColumnFilter",
                filterParams: {
                    filterOptions: TEXT_OPTS,
                    maxNumConditions: 1,
                },
            },
            opts || {},
        );
    }

    function num(field, header, opts) {
        return Object.assign(
            {
                headerName: header,
                field: field,
                filter: "agNumberColumnFilter",
                filterParams: { filterOptions: NUM_OPTS, maxNumConditions: 1 },
            },
            opts || {},
        );
    }

    function dt(field, header, opts) {
        return Object.assign(
            {
                headerName: header,
                field: field,
                valueFormatter: fmtDt,
                minWidth: 175,
                filter: "agTextColumnFilter",
                filterParams: {
                    filterOptions: ["startsWith", "contains", "equals"],
                    maxNumConditions: 1,
                },
            },
            opts || {},
        );
    }

    // A display-only column the backend can't sort/filter (e.g. JOINed columns
    // not in logservice's searchable field allowlist).
    function display(field, header, opts) {
        return Object.assign(
            { headerName: header, field: field, sortable: false, filter: false },
            opts || {},
        );
    }

    // ---- grid factory ------------------------------------------------------
    function create(config) {
        var url = config.url;
        var elId = config.elementId || "grid";

        var dataSource = {
            getRows: function (params) {
                var block = params.endRow - params.startRow;
                var req = { limit: block, offset: params.startRow };
                var search = buildSearch(params.filterModel);
                if (search.length) {
                    req.search = search;
                    req.searchLogic = "AND";
                }
                if (params.sortModel && params.sortModel.length) {
                    req.sort = params.sortModel.map(function (s) {
                        return { field: s.colId, direction: s.sort };
                    });
                }

                fetch(url + "?request=" + encodeURIComponent(JSON.stringify(req)))
                    .then(function (r) {
                        if (!r.ok) throw new Error("HTTP " + r.status);
                        return r.json();
                    })
                    .then(function (d) {
                        var rows = d.records || [];
                        // logservice returns the real grand total -> exact last
                        // row. Fall back to the short-block heuristic if absent.
                        var lastRow =
                            typeof d.total === "number"
                                ? d.total
                                : rows.length < block
                                  ? params.startRow + rows.length
                                  : undefined;
                        // The Infinite Row Model uses successCallback/failCallback
                        // (rows, lastRow); success/fail are Server-Side (Enterprise)
                        // only. Prefer the modern method when present.
                        if (typeof params.success === "function") {
                            params.success({ rowData: rows, rowCount: lastRow });
                        } else {
                            params.successCallback(rows, lastRow);
                        }
                    })
                    .catch(function () {
                        if (typeof params.fail === "function") params.fail();
                        else params.failCallback();
                    });
            },
        };

        agGrid.createGrid(document.getElementById(elId), {
            rowModelType: "infinite",
            datasource: dataSource,
            cacheBlockSize: PAGE_SIZE,
            maxBlocksInCache: 10,
            pagination: true,
            paginationPageSize: PAGE_SIZE,
            paginationPageSizeSelector: false,
            defaultColDef: {
                sortable: true,
                filter: true,
                floatingFilter: true,
                resizable: true,
                flex: 1,
                minWidth: 110,
            },
            columnDefs: config.columns,
        });
    }

    return {
        create: create,
        text: text,
        num: num,
        dt: dt,
        display: display,
    };
})();
