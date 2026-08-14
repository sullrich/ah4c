(function () {
	if (localStorage.getItem('ah4c-theme') === 'light') {
		document.documentElement.setAttribute('data-theme', 'light');
	}
	window.toggleTheme = function () {
		var light = document.documentElement.getAttribute('data-theme') === 'light';
		if (light) {
			document.documentElement.removeAttribute('data-theme');
			localStorage.setItem('ah4c-theme', 'dark');
		} else {
			document.documentElement.setAttribute('data-theme', 'light');
			localStorage.setItem('ah4c-theme', 'light');
		}
	};
	window.toggleNav = function () {
		var nav = document.querySelector('.pagebar .nav, .topbar .nav');
		if (nav) nav.classList.toggle('open');
	};
	window.addEventListener('storage', function (e) {
		if (e.key !== 'ah4c-theme') return;
		if (e.newValue === 'light') {
			document.documentElement.setAttribute('data-theme', 'light');
		} else {
			document.documentElement.removeAttribute('data-theme');
		}
	});
})();
