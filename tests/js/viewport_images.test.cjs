const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const tick = () => new Promise(resolve => setImmediate(resolve));
function setup() {
    let observer;
    const downloads=[], revoked=[], created=[];
    const item=()=>{
        const icon={querySelector:()=>({classList:{add(){},remove(){}}}),append(img){this.img=img;}};
        return {dataset:{fileId:String(Math.random()),url:'/image',favoriteName:'image'},icon,
            querySelector(selector){if(selector.includes('file-grid-icon'))return icon;
                if(selector.includes('thumb'))return {remove(){icon.img=null;}};
                return {classList:{remove(){}}};}};
    };
    const context={console, AbortController, URL:{createObjectURL(blob){created.push(blob);return 'blob:'+created.length;},revokeObjectURL(url){revoked.push(url);}},
        IntersectionObserver:class {constructor(fn){observer=fn;}observe(){}disconnect(){}},
        document:{querySelectorAll:()=>[],addEventListener(){},createElement:()=>({})},window:{addEventListener(){}},
        fetch:async()=>{downloads.push(1);throw Error('unexpected network');}};
    vm.runInNewContext(fs.readFileSync('mount123/internal/webserve/static/js/viewport_images.js','utf8'),context);
    const view=new context.CloudViewportImages({clientHeight:600},{textContent:''});
    return {view,item,observe:entries=>observer(entries),downloads,revoked,created};
}
test('leaving viewport releases thumbnail URL; returning restores local blob without downloading',async()=>{
    const {view,item,observe,downloads,revoked,created}=setup();
    const row=item();view.store={get:async()=>({size:100})};
    observe([{target:row,isIntersecting:true}]);await tick();
    assert.equal(view.urls.size,1);
    observe([{target:row,isIntersecting:false}]);
    assert.equal(view.urls.size,0);assert.equal(revoked.length,1);assert.equal(row.icon.img,null);
    observe([{target:row,isIntersecting:true}]);await tick();
    assert.equal(created.length,2);assert.equal(downloads.length,0);assert.equal(view.hits,2);
    view.close();assert.equal(view.urls.size,0);
});
test('two jobs at most; offscreen completions never reinstall decoded thumbnails',async()=>{
    const {view,item,observe}=setup();const rows=Array.from({length:5},item);const resolves=[];
    view.store={get:()=>new Promise(resolve=>resolves.push(resolve))};
    observe(rows.map(target=>({target,isIntersecting:true})));
    assert.equal(view.active,2);assert.equal(resolves.length,2);
    observe(rows.map(target=>({target,isIntersecting:false})));
    resolves.forEach(resolve=>resolve({size:100}));await tick();
    assert.equal(view.urls.size,0);assert.equal(view.active,0);assert.equal(view.queue.length,0);
});
