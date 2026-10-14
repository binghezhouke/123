/* A bounded, read-only view of the shared process. No cloud requests. */
(() => {
    'use strict';
    const number = value => typeof value === 'number' && Number.isFinite(value) && value >= 0;
    const delta = (before, after) => number(before) && number(after) && after >= before ? after - before : null;
    const ratio = (part, total) => number(part) && number(total) && total > 0 && part <= total ? part / total : null;
    const count = value => number(value) ? value.toLocaleString('zh-CN', {maximumFractionDigits: 1}) : '—';
    const bytes = value => {
        if (!number(value)) return '—';
        let unit = 0;
        while (value >= 1024 && unit < 4) { value /= 1024; unit++; }
        return `${count(Math.round(value * 10) / 10)} ${['B', 'KiB', 'MiB', 'GiB', 'TiB'][unit]}`;
    };
    const percent = value => number(value) ? `${(100 * value).toFixed(1)}%` : '—';
    const millis = value => number(value) ? `${count(Math.round(value * 100) / 100)} ms` : '—';
    const nanos = value => millis(number(value) ? value / 1e6 : null);
    const time = value => {
        if (!number(value)) return '—';
        const seconds = Math.floor(value);
        if (seconds >= 86400) return `${Math.floor(seconds / 86400)} 天 ${Math.floor(seconds % 86400 / 3600)} 小时`;
        if (seconds >= 3600) return `${Math.floor(seconds / 3600)} 小时 ${Math.floor(seconds % 3600 / 60)} 分`;
        return `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`;
    };
    // These hooked byte counters explicitly export unknown/null before their
    // first event. Missing groups are unavailable, not measured zero.
    const counterBytes = group => group?.status === 'unknown' && group.bytes === null ? 0 : group?.bytes;
    const durationCount = group => group?.status === 'unknown' && group.samples === null ? 0 : group?.samples;
    const durationTotal = group => group?.status === 'unknown' && group.total_nanos === null ? 0 : group?.total_nanos;
    const mean = group => number(group?.samples) && group.samples > 0 && number(group.total_nanos) ? group.total_nanos / group.samples : null;
    const intervalMean = (before, after) => {
        const samples = delta(durationCount(before), durationCount(after));
        const total = delta(durationTotal(before), durationTotal(after));
        return number(samples) && samples > 0 && number(total) ? total / samples : null;
    };
    function compare(before, after) {
        if (!before || !after || !before.io.started_at || before.io.started_at !== after.io.started_at) return null;
        const seconds = after.io.uptime_seconds - before.io.uptime_seconds;
        if (!(seconds > 0) || !Number.isFinite(seconds)) return null;
        const downloaded = delta(counterBytes(before.io.downloaded_bytes), counterBytes(after.io.downloaded_bytes));
        const sent = delta(before.web.response_bytes, after.web.response_bytes);
        // A restarted Web server can share an existing tree. Treat decreasing
        // completed/byte counters as a new epoch too.
        if (delta(before.web.completed, after.web.completed) === null || sent === null) return null;
        const pf = before.io.cache?.foreground, cf = after.io.cache?.foreground;
        const requested = pf?.status === 'measured' && cf?.status === 'measured' ? delta(pf.requested_bytes, cf.requested_bytes) : null;
        const hit = pf?.status === 'measured' && cf?.status === 'measured' ? delta(pf.hit_bytes, cf.hit_bytes) : null;
        return {seconds, download_rate: number(downloaded) ? downloaded / seconds : null, web_rate: sent / seconds, hit_ratio: ratio(hit, requested)};
    }

    class History {
        constructor(limit = 360) { this.limit = limit; this.points = []; this.previous = null; this.latest = null; }
        gap() { this.previous = null; }
        add(snapshot) {
            if (snapshot?.schema_version !== 1 || !snapshot.io || !snapshot.web || !snapshot.runtime || !Number.isFinite(Date.parse(snapshot.io.collected_at))) throw new Error('统计数据格式不兼容');
            if (this.latest && snapshot.io.started_at !== this.latest.io.started_at) { this.points = []; this.gap(); }
            if (this.latest?.io.started_at === snapshot.io.started_at && this.latest.io.collected_at === snapshot.io.collected_at) return null;
            const before = this.previous;
            const interval = compare(before, snapshot);
            const scheduler = snapshot.io.download_scheduler, queue = snapshot.io.index_queue;
            this.points.push({at: Date.parse(snapshot.io.collected_at), break_before: !interval, ...interval,
                download_active: scheduler?.status === 'measured' ? scheduler.active_requests : null,
                download_waiting: scheduler?.status === 'measured' ? scheduler.waiting_foreground + scheduler.waiting_background : null,
                index_waiting: queue?.status === 'measured' ? queue.waiting : null});
            if (this.points.length > this.limit) this.points.splice(0, this.points.length - this.limit);
            this.latest = snapshot; this.previous = snapshot;
            return {before: interval ? before : null, interval};
        }
    }
    if (typeof module !== 'undefined' && module.exports) module.exports = {compare, intervalMean, History, bytes, percent};
    if (typeof document === 'undefined') return;

    const $ = id => document.getElementById(id);
    const set = (id, value) => { $(id).textContent = value; };
    const history = new History();
    let paused = false, stopped = false, timer = null, controller = null, failed = false;
    let lastInterval = null;

    function facts(id, pairs) {
        const fragment = document.createDocumentFragment();
        for (const [label, value] of pairs) {
            const row = document.createElement('div'); row.className = 'fact';
            const name = document.createElement('span'); name.textContent = label;
            const number = document.createElement('strong'); number.textContent = value;
            row.append(name, number); fragment.append(row);
        }
        $(id).replaceChildren(fragment);
    }
    function table(id, rows) {
        const fragment = document.createDocumentFragment();
        for (const values of rows) {
            const row = document.createElement('tr');
            for (const value of values) { const cell = document.createElement('td'); cell.textContent = value; row.append(cell); }
            fragment.append(row);
        }
        $(id).replaceChildren(fragment);
    }
    const apiNames = {list: '目录列表', file_info: '文件信息', download_url: '下载直链', access_token: '访问令牌', other: '其他'};
    const stageNames = {directory_lookup: '目录查找', source_prepare: '读取源准备', url_resolve: '直链解析', source_probe: '源探测', build_queue: '构建排队', archive_index: '压缩包索引', decompression: '解压', file_open: '文件打开'};
    const latencyNames = {foreground_read_latency: 'FUSE 前台读取等待', foreground_read_success_time: 'FUSE 成功读取', transfer_queue_latency: '源下载排队', http_body_ttfb: '源响应正文首字节', http_transfer_latency: '源正文传输', cache_publication_latency: '缓存发布', read_ahead_foreground_wait: '预读前台等待'};
    const classNames = {speculative: '预取', probation: '初次使用', hot: '频繁使用', index: '受保护索引'};
    const kindNames = {archive_index: '压缩包索引', directory_snapshot: '目录快照', remote_identity: '来源身份', archive_probe: '格式探测', unclassified_index: '其他索引'};
    const measured = (group, key) => group?.status === 'measured' ? group[key] : null;
    function render(snapshot, before, interval) {
        const io = snapshot.io, web = snapshot.web, runtime = snapshot.runtime;
        const c = io.cache || {}, scheduler = io.download_scheduler || {}, q = io.index_queue || {};
        set('download-rate', number(interval?.download_rate) ? `${bytes(interval.download_rate)}/s` : '—');
        set('web-rate', number(interval?.web_rate) ? `${bytes(interval.web_rate)}/s` : '—');
        set('cache-hit', percent(interval?.hit_ratio)); set('web-active', count(web.active));
        set('sample-time', `采样于 ${new Date(io.collected_at).toLocaleTimeString()}${interval ? ` · 区间 ${interval.seconds.toFixed(1)} 秒` : ' · 等待下一个区间采样'}`);
        set('cache-capacity', `${bytes(c.used_bytes)} / ${bytes(c.capacity_bytes)}`);
        $('cache-usage').value = (ratio(c.used_bytes, c.capacity_bytes) || 0) * 100;
        facts('cache-summary', [['单个远端文件额度', c.source_limit_percent === 0 ? '未限制' : `${count(c.source_limit_percent)}% · ${bytes(c.source_limit_bytes)}`], ['最大来源常驻 / 预留', `${bytes(c.largest_source_bytes)} / ${bytes(c.largest_source_reserved_bytes)}`], ['来源数 / 活跃暂超额', `${count(c.source_count)} / ${count(c.source_over_limit_count)}`], ['来源额度淘汰 / 字节', `${count(c.source_evictions)} / ${bytes(c.source_eviction_bytes)}`], ['旧缓存待归属', bytes(c.unattributed_data_bytes)], ['条目数', count(c.entries)], ['正在使用', bytes(c.pinned_bytes)], ['填充预留', bytes(c.reserved_bytes)], ['索引占用 / 预算', `${bytes(c.index_used_bytes)} / ${bytes(c.index_budget_bytes)}`], ['填充成功', count(c.fill_successes)], ['填充取消', count(c.fill_outcome_version === 1 ? c.fill_cancelled || 0 : null)], ['填充错误', count(c.fill_outcome_version === 1 ? c.fill_errors || 0 : null)], ['等待已有填充', count(c.existing_fill_waits)], ['淘汰总量', bytes(c.capacity_eviction_bytes)], ['淘汰后再次请求', count(c.refaults?.count)]]);
        table('cache-classes', (c.classes || []).map(row => [classNames[row.class] || row.class, count(row.entries), bytes(row.bytes), bytes(row.pinned_bytes), count(row.capacity_evictions), bytes(row.capacity_eviction_bytes)]));
        table('cache-ranges', [['前台', c.foreground], ['后台', c.background]].map(([name, row]) => [name, count(measured(row, 'read_requests')), count(measured(row, 'full_hits')), bytes(measured(row, 'requested_bytes')), bytes(measured(row, 'hit_bytes')), percent(ratio(measured(row, 'hit_bytes'), measured(row, 'requested_bytes')))]));
        facts('queues', [['下载活跃 / 上限', `${count(measured(scheduler, 'active_requests'))} / ${count(measured(scheduler, 'maximum_requests'))}`], ['前台 / 后台等待', `${count(measured(scheduler, 'waiting_foreground'))} / ${count(measured(scheduler, 'waiting_background'))}`], ['下载占用 / 字节预算', `${bytes(measured(scheduler, 'active_bytes'))} / ${bytes(measured(scheduler, 'maximum_in_flight_bytes'))}`], ['后台可用预算', bytes(measured(scheduler, 'available_background_bytes'))], ['暂存缓冲 / 峰值', `${bytes(measured(scheduler, 'staging_active_bytes'))} / ${bytes(measured(scheduler, 'staging_peak_bytes'))}`], ['暂存前台 / 后台等待', `${count(measured(scheduler, 'staging_waiting_foreground'))} / ${count(measured(scheduler, 'staging_waiting_background'))}`], ['累计下载请求完成', count(measured(scheduler, 'completed_requests'))], ['索引活跃 / 上限', `${count(measured(q, 'active'))} / ${count(measured(q, 'limit'))}`], ['索引后台活跃 / 等待', `${count(measured(q, 'background_active'))} / ${count(measured(q, 'waiting'))}`]]);
        facts('web-summary', [['活跃响应', count(web.active)], ['已完成请求', count(web.completed)], ['已写入响应字节', bytes(web.response_bytes)], ['客户端取消', count(web.cancelled)], ['2xx / 3xx', `${count(web.success)} / ${count(web.redirect)}`], ['4xx / 5xx', `${count(web.client_error)} / ${count(web.server_error)}`], ['完成请求平均耗时', nanos(web.completed > 0 ? web.total_nanos / web.completed : null)], ['完成请求最大耗时', nanos(web.completed > 0 ? web.max_nanos : null)]]);
        table('api-table', Object.entries(apiNames).map(([key, name]) => {
            const a = io.api_profile?.[key], b = before?.io.api_profile?.[key];
            const requests = delta(b?.requests, a?.requests);
            return [name, count(a?.requests), count(a?.errors), count(a?.retries), count(a?.throttle_waits), count(interval && number(requests) ? requests / interval.seconds : null), nanos(intervalMean(b?.latency, a?.latency)), nanos(mean(a?.latency)), nanos(a?.latency?.p95_nanos), nanos(a?.latency?.max_nanos)];
        }));
        const durations = Object.entries(stageNames).map(([key, name]) => [name, io.stages?.[key], before?.io.stages?.[key]]).concat(Object.entries(latencyNames).map(([key, name]) => [name, io[key], before?.io[key]]));
        table('latency-table', durations.map(([name, a, b]) => [name, count(a?.samples), nanos(intervalMean(b, a)), nanos(mean(a)), nanos(a?.p50_nanos), nanos(a?.p95_nanos), nanos(a?.max_nanos)]));
        const dc = io.directory_cache, rr = io.remote_recovery;
        facts('directory-summary', [['内存命中', count(measured(dc, 'memory_hits'))], ['磁盘恢复 / 尝试', `${count(measured(dc, 'disk_restores'))} / ${count(measured(dc, 'disk_restore_attempts'))}`], ['云列表调用 / 错误', `${count(measured(dc, 'list_calls'))} / ${count(measured(dc, 'list_errors'))}`], ['返回旧快照 / 刷新失败', `${count(measured(dc, 'stale_served'))} / ${count(measured(dc, 'stale_failures'))}`], ['列表重试退避', count(measured(dc, 'retry_backoffs'))], ['损坏快照 / 保存失败', `${count(measured(dc, 'corrupt_snapshots'))} / ${count(measured(dc, 'persistence_failures'))}`], ['永久目录错误', count(measured(dc, 'permanent_failures'))], ['Range 恢复尝试 / 重试', `${count(measured(rr, 'attempts'))} / ${count(measured(rr, 'retries'))}`], ['恢复成功 / 耗尽', `${count(measured(rr, 'recovered'))} / ${count(measured(rr, 'exhausted'))}`], ['恢复取消 / 补读字节', `${count(measured(rr, 'cancelled'))} / ${bytes(measured(rr, 'suffix_bytes'))}`]]);
        const pf = io.image_prefetch;
        facts('prefetch-summary', [['源累计下载', bytes(io.downloaded_bytes?.bytes)], ['预读计划', bytes(io.read_ahead_scheduled_bytes?.bytes)], ['预读完成', bytes(io.read_ahead_completed_bytes?.bytes)], ['预读被消费', bytes(io.read_ahead_consumed_bytes?.bytes)], ['关闭时未用的预读', bytes(io.read_ahead_wasted_bytes?.bytes)], ['图片计划 / 开始', `${count(measured(pf, 'planned'))} / ${count(measured(pf, 'started'))}`], ['图片复用 / 完成', `${count(measured(pf, 'reused'))} / ${count(measured(pf, 'completed'))}`], ['图片取消 / 失败', `${count(measured(pf, 'cancelled'))} / ${count(measured(pf, 'failed'))}`], ['前台已准备 / 在途', `${count(measured(pf, 'foreground_ready'))} / ${count(measured(pf, 'foreground_in_flight'))}`]]);
        const scan = io.scan;
        facts('scan-summary', [['后台扫描降载', scan?.status === 'measured' ? scan.shedding ? '正在降载' : '未触发' : '—'], ['元数据请求 / 秒', count(measured(scan, 'metadata_requests_per_second'))], ['进入 / 退出阈值', `${count(measured(scan, 'enter_requests_per_second'))} / ${count(measured(scan, 'exit_requests_per_second'))}`]]);
        table('operations-table', ['readdir', 'lookup', 'getattr', 'open'].map(key => [key, ...['requests', 'foreground_requests', 'background_requests', 'active_foreground', 'active_background'].map(field => count(measured(io[key], field)))]));
        facts('disk-summary', [['磁盘剩余空间', bytes(c.disk_free_bytes)], ['最低保留空间', bytes(c.disk_minimum_free_bytes)], ['空间压力事件', count(c.disk_pressure_events)], ['空间不足拒绝 / ENOSPC', `${count(c.disk_pressure_rejects)} / ${count(c.enospc)}`], ['磁盘压力淘汰', count(c.disk_pressure_evictions)], ['孤立临时文件清理', count(c.orphan_files_cleaned)]]);
        table('index-table', (c.index_kinds || []).map(row => [kindNames[row.kind] || row.kind, count(row.entries), bytes(row.bytes), bytes(row.pinned_bytes), count(row.over_budget_demotions), count(row.capacity_evictions)]));
        const cfg = io.configuration || {};
        facts('runtime-summary', [['版本 / 提交', `${runtime.version || '—'} / ${runtime.commit || '—'}`], ['构建时间', runtime.build_time || '—'], ['运行时长', time(io.uptime_seconds)], ['Go 版本', runtime.go_version || '—'], ['Goroutine 数', count(runtime.goroutines)], ['Go 堆使用 / 保留', `${bytes(runtime.heap_bytes)} / ${bytes(runtime.heap_system_bytes)}`], ['GC 次数', count(runtime.gc_count)], ['目录快照 TTL', time(cfg.directory_ttl_seconds)], ['读取源复用 TTL', time(cfg.source_ttl_seconds)], ['元数据内存预算', bytes(cfg.metadata_bytes)], ['连续预读上限', bytes(cfg.read_ahead_max_bytes)], ['预取文件数 / 并发', `${count(cfg.prefetch_files)} / ${count(cfg.prefetch_workers)}`], ['预取字节预算', bytes(cfg.prefetch_bytes)]]);
        $('export').disabled = false;
    }

    const chartConfigs = [
        ['chart-traffic', [['download_rate', '网盘下载', 'blue', 1 / 1048576], ['web_rate', 'Web 响应', 'green', 1 / 1048576]], null],
        ['chart-cache', [['hit_ratio', '前台命中率', 'blue', 100]], 100],
        ['chart-queue', [['download_active', '下载活跃', 'blue', 1], ['download_waiting', '下载等待', 'green', 1], ['index_waiting', '索引等待', 'orange', 1]], null]
    ];
    const svgNS = 'http://www.w3.org/2000/svg';
    function svgElement(tag, attrs, text) {
        const node = document.createElementNS(svgNS, tag);
        for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
        if (text !== undefined) node.textContent = text;
        return node;
    }
    function drawCharts() {
        const end = history.points.at(-1)?.at;
        const points = history.points.filter(p => p.at >= end - Number($('history-window').value));
        for (const [id, series, fixedMax] of chartConfigs) {
            const host = $(id), width = 460, height = 160, left = 40, right = 450, top = 12, bottom = 130;
            const maxY = fixedMax || Math.max(1, ...points.flatMap(p => series.map(([key, , , scale]) => number(p[key]) ? p[key] * scale : 0))) * 1.1;
            const start = points[0]?.at || 0, span = Math.max(5000, (end || 0) - start);
            const x = p => left + (p.at - start) / span * (right - left);
            const y = v => bottom - v / maxY * (bottom - top);
            const svg = svgElement('svg', {viewBox: `0 0 ${width} ${height}`, role: 'img', 'aria-label': host.getAttribute('aria-label')});
            for (let n = 0; n <= 2; n++) {
                const value = maxY * n / 2, yy = y(value);
                svg.append(svgElement('line', {x1: left, x2: right, y1: yy, y2: yy, stroke: 'var(--line)'}));
                svg.append(svgElement('text', {x: left - 6, y: yy + 4, 'text-anchor': 'end', fill: 'var(--muted)', 'font-size': 10}, count(Math.round(value * 10) / 10)));
            }
            for (const [key, , color, scale] of series) {
                let path = '', pen = false;
                for (const point of points) {
                    if (point.break_before) pen = false;
                    if (!number(point[key])) { pen = false; continue; }
                    path += `${pen ? 'L' : 'M'}${x(point).toFixed(2)},${y(point[key] * scale).toFixed(2)} `; pen = true;
                    svg.append(svgElement('circle', {cx: x(point), cy: y(point[key] * scale), r: 1.6, fill: `var(--${color})`}));
                }
                svg.append(svgElement('path', {d: path, fill: 'none', stroke: `var(--${color})`, 'stroke-width': 2, 'stroke-linejoin': 'round'}));
            }
            if (points.length) {
                svg.append(svgElement('text', {x: left, y: 151, fill: 'var(--muted)', 'font-size': 10}, new Date(start).toLocaleTimeString()));
                svg.append(svgElement('text', {x: right, y: 151, fill: 'var(--muted)', 'font-size': 10, 'text-anchor': 'end'}, new Date(end).toLocaleTimeString()));
            }
            const cursor = svgElement('line', {y1: top, y2: bottom, stroke: 'var(--muted)', 'stroke-dasharray': '3 3', visibility: 'hidden'});
            svg.append(cursor);
            const readout = document.createElement('div'); readout.className = 'chart-readout'; readout.textContent = '等待采样；移动指针或按左右键查看数据';
            let selected = Math.max(0, points.length - 1);
            const show = index => {
                if (!points[index]) return;
                selected = index;
                const p = points[index]; cursor.setAttribute('x1', x(p)); cursor.setAttribute('x2', x(p)); cursor.setAttribute('visibility', 'visible');
                readout.textContent = `${new Date(p.at).toLocaleTimeString()} · ${series.map(([key, name, , scale]) => `${name} ${count(number(p[key]) ? Math.round(p[key] * scale * 100) / 100 : null)}`).join(' · ')}`;
            };
            svg.addEventListener('pointermove', event => {
                const rect = svg.getBoundingClientRect(), offset = (event.clientX - rect.left) / rect.width * width;
                const timestamp = start + Math.max(0, Math.min(1, (offset - left) / (right - left))) * span;
                if (points.length) show(points.reduce((best, p, i) => Math.abs(p.at - timestamp) < Math.abs(points[best].at - timestamp) ? i : best, 0));
            });
            host.onkeydown = event => {
                if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') { event.preventDefault(); show(Math.max(0, Math.min(points.length - 1, selected + (event.key === 'ArrowLeft' ? -1 : 1)))); }
            };
            host.replaceChildren(svg, readout);
        }
    }

    function connection() {
        const message = stopped ? '页面已离开' : document.hidden ? '标签页在后台，已暂停采样' : paused ? '已暂停，保留最后一次统计' : failed ? '连接失败，等待重试' : history.latest ? '实时更新中' : '正在获取统计…';
        set('connection', message);
        $('live-dot').className = `live-dot${failed ? ' error' : !paused && !document.hidden && !stopped && history.latest ? ' live' : ''}`;
        $('pause').textContent = paused ? '继续' : '暂停';
        $('pause').setAttribute('aria-pressed', String(paused));
    }
    function schedule() {
        clearTimeout(timer);
        if (!paused && !stopped && !document.hidden) timer = setTimeout(() => poll(), Number($('poll-interval').value));
        connection();
    }
    async function poll() {
        if (controller || stopped || document.hidden) return;
        clearTimeout(timer);
        const request = new AbortController(); controller = request;
        $('refresh').disabled = true;
        const deadline = setTimeout(() => request.abort(), 10000);
        try {
            const response = await fetch('/api/status', {cache: 'no-store', headers: {Accept: 'application/json'}, signal: request.signal});
            if (!response.ok) throw new Error(response.status === 401 ? '需要登录，请返回文件浏览页登录后重试' : `统计请求失败（HTTP ${response.status}）`);
            const snapshot = await response.json();
            if (request.signal.aborted) return;
            const result = history.add(snapshot);
            if (result) { lastInterval = result.interval; render(snapshot, result.before, result.interval); drawCharts(); }
            failed = false; $('status-error').hidden = true;
        } catch (error) {
            history.gap();
            if (!stopped && !document.hidden && !paused) {
                failed = true; $('status-error').hidden = false;
                $('status-error').textContent = `${error.name === 'AbortError' ? '统计请求超时' : error.message}。保留上次结果，将自动重试。`;
                set('download-rate', '—'); set('web-rate', '—'); set('cache-hit', '—');
            }
        } finally {
            clearTimeout(deadline); controller = null; $('refresh').disabled = false; schedule();
        }
    }
    $('pause').addEventListener('click', () => { paused = !paused; history.gap(); controller?.abort(); clearTimeout(timer); if (!paused && !controller) poll(); else schedule(); });
    $('refresh').addEventListener('click', () => poll());
    $('poll-interval').addEventListener('change', schedule);
    $('history-window').addEventListener('change', drawCharts);
    $('export').addEventListener('click', () => {
        const payload = {snapshot: history.latest, interval: lastInterval, history: history.points};
        const url = URL.createObjectURL(new Blob([JSON.stringify(payload, null, 2)], {type: 'application/json'}));
        const anchor = document.createElement('a'); anchor.href = url; anchor.download = 'mount123-status.json'; document.body.append(anchor); anchor.click(); anchor.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    });
    document.addEventListener('visibilitychange', () => { clearTimeout(timer); history.gap(); if (document.hidden) controller?.abort(); else if (!paused) poll(); connection(); });
    window.addEventListener('pagehide', () => { stopped = true; clearTimeout(timer); controller?.abort(); history.gap(); });
    window.addEventListener('pageshow', event => { if (event.persisted) { stopped = false; if (!paused) poll(); connection(); } });
    poll();
})();
