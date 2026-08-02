const url = new URL(window.location);
const usp = url.searchParams;

const params = usp.getAll("msg");
const hasmsg = usp.has("msg");

if (hasmsg) {
    console.log("invalid auth");
    const el = document.getElementById("alert");
    console.log(el);
    el.style.display = "block";
}

// console.log(url);
// console.log(usp);
// console.log(params);
// console.log(hasmsg);
