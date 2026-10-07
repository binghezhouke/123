/* Panzoom owns gestures; fit/original sizing and rotation live here. */
window.createArchiveImageViewport = function (dialog) {
    const el = name => document.getElementById(`zip-gallery-${name}`), stage = el('stage');
    let panzoom = null, image = null, expanded = false, ownsFullscreen = false, switchingFullscreen = false, rotation = 0, originalSize = false;
    function updateZoom() {
        const scale = panzoom?.getScale() || 1;
        el('zoom-in').disabled = !panzoom || scale >= 8; el('zoom-out').disabled = !panzoom || scale <= 1.001;
        el('zoom-reset').disabled = !panzoom; el('zoom-level').textContent = `${Math.round(scale * 100)}%`;
        stage.classList.toggle('is-zoomed', scale > 1.001 || originalSize);
    }
    function applyRotation() {
        if (!image) return;
        const quarterTurn = rotation % 180 !== 0;
        image.style.maxWidth = originalSize ? 'none' : `${quarterTurn ? stage.clientHeight : stage.clientWidth}px`;
        image.style.maxHeight = originalSize ? 'none' : `${quarterTurn ? stage.clientWidth : stage.clientHeight}px`;
        const base = image.style.transform.replace(/\s*rotate\(\d+deg\)/g, '');
        image.style.transform = `${base} rotate(${rotation}deg)`;
    }
    function reset() { const wasOriginalSize=originalSize; originalSize = false; stage.classList.remove('gallery-original'); if(wasOriginalSize)panzoom?.setOptions({panOnlyWhenZoomed:true}); applyRotation(); panzoom?.reset({animate:false}); updateZoom(); }
    function onPanzoomChange() { applyRotation(); updateZoom(); }
    function detach() { image?.removeEventListener('panzoomchange', onPanzoomChange); panzoom?.destroy(); panzoom?.resetStyle(); panzoom=null; image=null; rotation=0; originalSize=false; stage.classList.remove('gallery-original'); updateZoom(); }
    function setExpanded(value) { expanded=value; dialog.classList.toggle('gallery-expanded', value); el('fullscreen').textContent=value?'退出全屏':'全屏'; el('fullscreen').setAttribute('aria-pressed',String(value)); reset(); }
    async function exitFullscreen() { setExpanded(false); if (ownsFullscreen && document.fullscreenElement) { ownsFullscreen=false; try {await document.exitFullscreen();} catch {} } ownsFullscreen=false; }
    el('fullscreen').addEventListener('click', async () => { if(switchingFullscreen)return; switchingFullscreen=true; try { if(expanded){await exitFullscreen();return;} setExpanded(true); if(!document.fullscreenElement&&document.documentElement.requestFullscreen){try{await document.documentElement.requestFullscreen();ownsFullscreen=true;if(!dialog.open)await exitFullscreen();}catch{}} } finally{switchingFullscreen=false;} });
    document.addEventListener('fullscreenchange',()=>{if(ownsFullscreen&&!document.fullscreenElement){ownsFullscreen=false;setExpanded(false);}});
    el('zoom-in').addEventListener('click',()=>panzoom?.zoomIn({animate:false})); el('zoom-out').addEventListener('click',()=>panzoom?.zoomOut({animate:false})); el('zoom-reset').addEventListener('click',reset);
    el('original')?.addEventListener('click',()=>{if(!image)return; originalSize=true; stage.classList.add('gallery-original'); applyRotation(); panzoom?.reset({animate:false}); panzoom?.setOptions({panOnlyWhenZoomed:false}); updateZoom();});
    el('rotate')?.addEventListener('click',()=>{if(!image)return; rotation=(rotation+90)%360; applyRotation(); panzoom?.reset({animate:false}); updateZoom();});
    stage.addEventListener('wheel',event=>{if(panzoom)panzoom.zoomWithWheel(event);},{passive:false});
    stage.addEventListener('dblclick',event=>{if(!panzoom)return;event.preventDefault();if(panzoom.getScale()>1.001)reset();else panzoom.zoomToPoint(2,event,{animate:false});});
    new ResizeObserver(()=>{if(dialog.open)reset();}).observe(stage); window.addEventListener('pagehide',()=>{detach();exitFullscreen();}); updateZoom();
    return {attach(element){detach();if(typeof Panzoom!=='function')return;image=element;image.draggable=false;panzoom=Panzoom(image,{canvas:true,minScale:1,maxScale:8,panOnlyWhenZoomed:true,pinchAndPan:true,animate:false});image.addEventListener('panzoomchange',onPanzoomChange);applyRotation();updateZoom();},detach,isZoomed:()=>originalSize||(panzoom?.getScale()||1)>1.001,close(){detach();exitFullscreen();}};
};
