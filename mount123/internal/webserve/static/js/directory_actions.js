/* Native disclosure handles activation and announces its expanded state. */
(() => {
    const actions = document.querySelector('.directory-actions');
    if (!actions) return;
    const trigger = actions.querySelector('summary');

    document.addEventListener('click', event => {
        if (!actions.contains(event.target)) actions.open = false;
    });

    actions.addEventListener('keydown', event => {
        if (event.key !== 'Escape' || !actions.open) return;
        event.preventDefault();
        event.stopPropagation();
        actions.open = false;
        trigger.focus();
    });

    actions.addEventListener('focusout', event => {
        if (!actions.contains(event.relatedTarget)) actions.open = false;
    });

    window.addEventListener('pagehide', () => { actions.open = false; });
})();
