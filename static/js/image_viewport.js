/* Panzoom owns image gestures; the gallery continues to own loading and navigation. */
window.createArchiveImageViewport = function (dialog) {
    const el = name => document.getElementById(`zip-gallery-${name}`);
    const stage = el('stage');
    let panzoom = null, image = null;
    let expanded = false, ownsFullscreen = false, switchingFullscreen = false;
    function updateZoom() {
        const scale = panzoom?.getScale() || 1;
        el('zoom-in').disabled = !panzoom || scale >= 8;
        el('zoom-out').disabled = !panzoom || scale <= 1.001;
        el('zoom-reset').disabled = !panzoom;
        el('zoom-level').textContent = `${Math.round(scale * 100)}%`;
        stage.classList.toggle('is-zoomed', scale > 1.001);
    }
    function reset() {panzoom?.reset({animate: false}); updateZoom();}
    function detach() {
        image?.removeEventListener('panzoomchange', updateZoom);
        panzoom?.destroy();
        panzoom?.resetStyle();
        panzoom = null; image = null;
        updateZoom();
    }
    function setExpanded(value) {
        expanded = value;
        dialog.classList.toggle('gallery-expanded', value);
        el('fullscreen').textContent = value ? '退出全屏' : '全屏';
        el('fullscreen').setAttribute('aria-pressed', String(value));
        reset();
    }
    async function exitFullscreen() {
        setExpanded(false);
        if (ownsFullscreen && document.fullscreenElement) {
            ownsFullscreen = false;
            try {await document.exitFullscreen();} catch { /* Browser may already be exiting. */ }
        }
        ownsFullscreen = false;
    }
    el('fullscreen').addEventListener('click', async () => {
        if (switchingFullscreen) return;
        switchingFullscreen = true;
        try {
            if (expanded) {await exitFullscreen(); return;}
            setExpanded(true);
            // A <dialog> cannot be a Fullscreen API target; expand the document,
            // while the modal dialog covers the viewport and keeps focus trapped.
            if (!document.fullscreenElement && document.documentElement.requestFullscreen) {
                try {
                    await document.documentElement.requestFullscreen();
                    ownsFullscreen = true;
                    if (!dialog.open) await exitFullscreen();
                } catch {
                    // Mobile/embedded browsers may deny native fullscreen.
                    // The viewport-filling dialog remains fully usable.
                }
            }
        } finally {switchingFullscreen = false;}
    });
    document.addEventListener('fullscreenchange', () => {
        if (ownsFullscreen && !document.fullscreenElement) {
            ownsFullscreen = false;
            setExpanded(false);
        }
    });
    el('zoom-in').addEventListener('click', () => panzoom?.zoomIn({animate: false}));
    el('zoom-out').addEventListener('click', () => panzoom?.zoomOut({animate: false}));
    el('zoom-reset').addEventListener('click', reset);
    stage.addEventListener('wheel', event => {if (panzoom) panzoom.zoomWithWheel(event);}, {passive: false});
    stage.addEventListener('dblclick', event => {
        if (!panzoom) return;
        event.preventDefault();
        if (panzoom.getScale() > 1.001) reset();
        else panzoom.zoomToPoint(2, event, {animate: false});
    });
    new ResizeObserver(() => {if (dialog.open) reset();}).observe(stage);
    window.addEventListener('pagehide', () => {detach(); exitFullscreen();});
    updateZoom();
    return {
        attach(element) {
            detach();
            if (typeof Panzoom !== 'function') return;
            image = element;
            image.draggable = false;
            panzoom = Panzoom(image, {canvas: true, minScale: 1, maxScale: 8,
                panOnlyWhenZoomed: true, pinchAndPan: true, animate: false});
            image.addEventListener('panzoomchange', updateZoom);
            updateZoom();
        },
        detach,
        isZoomed: () => (panzoom?.getScale() || 1) > 1.001,
        close() {detach(); exitFullscreen();},
    };
};
