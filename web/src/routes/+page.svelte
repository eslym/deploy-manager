<svelte:head>
	<title>Deploy Manager</title>
	<meta name="description" content="Sign in to manage deployments and releases." />
</svelte:head>

<main class="mx-auto my-[12vh] w-[min(100%-2rem,34rem)]">
	<p class="text-[0.8rem] font-bold tracking-[0.12em] text-[#637166] uppercase">Deploy Manager</p>
	<h1 class="mt-2 mb-4 text-[clamp(2rem,8vw,3.5rem)] tracking-[-0.04em]">
		Manage deployments and releases
	</h1>
	<p class="leading-[1.6] text-[#58645b]">
		Sign in to review deployments, publish releases, and manage your projects.
	</p>
	<a
		class="mt-6 inline-flex rounded-md bg-[#214c30] px-5 py-3 font-semibold text-white underline-offset-4 hover:bg-[#183b24] focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#214c30]"
		href="/login">Sign in</a
	>
	<p class="mt-4 text-sm">
		<a class="text-[#58645b] underline underline-offset-4 hover:text-[#214c30]" href="/register">
			Open administrator setup
		</a>
	</p>
</main>
